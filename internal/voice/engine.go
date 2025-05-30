package voice

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
VoiceEngine — единый движок озвучки и фоновой мелодии.
Использование:

	v  := voice.New(voice.Config{Voice:"Milena", Rate:200, Volume:0.8})
	v.MelodyOn()
	v.Speak(voice.Normal, "Текст")
	…
	v.Shutdown()
*/
type Priority int

const (
	System Priority = iota // немедленно, выше всех
	High                   // в голову очереди
	Normal                 // FIFO
	Low                    // слабый, может быть отброшен
)

type Config struct {
	Voice        string
	Rate         int
	Volume       float64       // 0‥1
	FadeMs       int           // длительность fade-in/out (мс)
	MaxQueue     int           // размер очереди
	DebounceSame time.Duration // анти-спам одинаковых фраз
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
	if c.FadeMs == 0 {
		c.FadeMs = 500
	}
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

// Speak  — озвучить текст (по приоритету).
func (e *Engine) Speak(p Priority, text string) {
	if text == "" {
		return
	}

	e.mu.Lock()
	if t, ok := e.lastSpoken[text]; ok && time.Since(t) < e.cfg.DebounceSame {
		e.mu.Unlock()
		return // недавно уже говорили то же самое
	}
	e.lastSpoken[text] = time.Now()
	e.mu.Unlock()

	m := message{txt: text, pr: p, t: time.Now()}
	select {
	case e.queue <- m:
	default:
		// очередь переполнена →
		if p <= High { // важное – проталкиваем, выталкивая Low/Normal
			<-e.queue
			e.queue <- m
		}
	}
}

// MelodyOn  – начать тихо крутить системную мелодию (loop).
func (e *Engine) MelodyOn() {
	if e.melodyCmd != nil && e.melodyCmd.ProcessState == nil {
		return // уже играет
	}
	e.melodyCmd = exec.CommandContext(
		e.ctx,
		"afplay",
		"-v", "0.4", // уровень afplay
		"/System/Library/Sounds/Submarine.aiff", // любой короткий WAV/AIFF
		"-q", "1", "-t", "999999",               // тихий режим, «бесконечно»
	)
	_ = e.melodyCmd.Start()
}

// MelodyOff  – полностью останавливает фон.
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
			e.fadeVolume(0.25)
			e.say(m.txt)
			e.fadeVolume(e.cfg.Volume)
		}
	}
}

func (e *Engine) say(text string) {
	args := []string{"-v", e.cfg.Voice, "-r", strconv.Itoa(e.cfg.Rate), text}
	_ = exec.CommandContext(e.ctx, "say", args...).Run()
}

/*------------------------------------------------------------------
			   FADE VOLUME helpers
------------------------------------------------------------------*/

func (e *Engine) fadeVolume(target float64) {
	cur, _ := e.getVolume()
	steps := 8
	delay := time.Duration(e.cfg.FadeMs/steps) * time.Millisecond
	diff := (target - cur) / float64(steps)
	for i := 0; i < steps; i++ {
		cur += diff
		e.setVolume(cur)
		time.Sleep(delay)
	}
}

func (e *Engine) getVolume() (float64, error) {
	out, err := exec.Command(
		"osascript", "-e", "output volume of (get volume settings)").Output()
	if err != nil {
		return 1, err
	}
	vStr := strings.TrimSpace(string(out))
	vInt, _ := strconv.Atoi(vStr)
	return float64(vInt) / 100.0, nil
}

func (e *Engine) setVolume(vol float64) {
	if vol < 0 {
		vol = 0
	}
	if vol > 1 {
		vol = 1
	}
	_ = exec.Command(
		"osascript", "-e",
		fmt.Sprintf("set volume output volume %d", int(vol*100))).Run()
}

func (e *Engine) stopMelody() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		_ = e.melodyCmd.Process.Kill()
		e.melodyCmd = nil
	}
}
