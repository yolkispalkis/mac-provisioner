package voice

import (
	"context"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

/*
VoiceEngine — единый движок озвучки и фоновой мелодии.
Теперь БЕЗ регулировки громкости.

	v := voice.New(voice.Config{Voice:"Milena", Rate:200})
	v.MelodyOn()
	v.Speak(voice.Normal, "Текст")
	…
	v.Shutdown()
*/
type Priority int

const (
	System Priority = iota // немедленно
	High                   // в голову очереди
	Normal                 // FIFO
	Low                    // может быть отброшен
)

type Config struct {
	Voice        string
	Rate         int
	MaxQueue     int
	DebounceSame time.Duration
}

type message struct {
	txt string
	pr  Priority
	t   time.Time
}

/*------------------------------------------------------------------*/

type Engine struct {
	cfg        Config
	queue      chan message
	lastSpoken map[string]time.Time
	mu         sync.Mutex

	melodyCmd *exec.Cmd
	ctx       context.Context
	cancel    context.CancelFunc
}

func New(c Config) *Engine {
	if c.MaxQueue == 0 {
		c.MaxQueue = 30
	}
	if c.DebounceSame == 0 {
		c.DebounceSame = 2 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		cfg:        c,
		queue:      make(chan message, c.MaxQueue),
		lastSpoken: map[string]time.Time{},
		ctx:        ctx,
		cancel:     cancel,
	}
	go e.runner()
	return e
}

/*------------------------------------------------------------------
	ПУБЛИЧНЫЙ API
------------------------------------------------------------------*/

func (e *Engine) Shutdown() { e.cancel(); e.stopMelody() }

// Speak озвучивает текст с заданным приоритетом.
func (e *Engine) Speak(p Priority, text string) {
	if text == "" {
		return
	}

	e.mu.Lock()
	if t, ok := e.lastSpoken[text]; ok && time.Since(t) < e.cfg.DebounceSame {
		e.mu.Unlock()
		return // анти-спам
	}
	e.lastSpoken[text] = time.Now()
	e.mu.Unlock()

	m := message{txt: text, pr: p, t: time.Now()}
	select {
	case e.queue <- m:
	default:
		// если очередь полна
		if p <= High { // важно → вытолкнём что-то менее важное
			<-e.queue
			e.queue <- m
		}
	}
}

// MelodyOn — запускает цикл afplay, если ещё не запущен.
func (e *Engine) MelodyOn() {
	if e.melodyCmd != nil && e.melodyCmd.ProcessState == nil {
		return // уже играет
	}
	e.melodyCmd = exec.CommandContext(
		e.ctx,
		"afplay",
		"/System/Library/Sounds/Submarine.aiff",
		"-q", "1", "-t", "999999",
	)
	_ = e.melodyCmd.Start()
}

// MelodyOff — полностью останавливает фон.
func (e *Engine) MelodyOff() { e.stopMelody() }

/*------------------------------------------------------------------
	ВНУТРЕННЯЯ МАШИНА
------------------------------------------------------------------*/

func (e *Engine) runner() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case m := <-e.queue:
			e.pauseMelody()
			e.say(m.txt)
			e.resumeMelody()
		}
	}
}

func (e *Engine) say(text string) {
	args := []string{"-v", e.cfg.Voice, "-r", strconv.Itoa(e.cfg.Rate), text}
	_ = exec.CommandContext(e.ctx, "say", args...).Run()
}

/*------------------------------------------------------------------
	              Melody Pause / Resume
------------------------------------------------------------------*/

func (e *Engine) pauseMelody() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		_ = e.melodyCmd.Process.Signal(syscall.SIGSTOP)
	}
}

func (e *Engine) resumeMelody() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		_ = e.melodyCmd.Process.Signal(syscall.SIGCONT)
	}
}

func (e *Engine) stopMelody() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		_ = e.melodyCmd.Process.Kill()
		e.melodyCmd = nil
	}
}
