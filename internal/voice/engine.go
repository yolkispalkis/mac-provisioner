package voice

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

/*
Использование:

        v := voice.New(voice.Config{
                Voice:  "Milena",
                Rate:   200,
                Volume: 0.8,          // ← новая настройка
        })
        v.MelodyOn()
        v.Speak(voice.Normal, "Текст")
        v.Shutdown()
*/

type Priority int

const (
	System Priority = iota // немедленно
	High                   // в голову очереди
	Normal                 // FIFO
	Low                    // может быть отброшен
)

/*------------------------------------------------------------------*/

type Config struct {
	Voice        string
	Rate         int
	Volume       float64       // ← добавлено: 0.0…1.0, влияет на MelodyOn
	MaxQueue     int           // ёмкость канала
	DebounceSame time.Duration // анти-спам для одинаковых фраз
	MergeWindow  time.Duration // «склейка» сообщений < MergeWindow
	MinInterval  time.Duration // пауза между say-процессами
}

// message во внутренней очереди
type message struct {
	txt string
	pr  Priority
	t   time.Time
}

/*------------------------------------------------------------------*/

type Engine struct {
	cfg Config

	queue chan message

	mu         sync.Mutex
	lastSpoken map[string]time.Time
	lastSay    time.Time

	melodyCmd *exec.Cmd
	ctx       context.Context
	cancel    context.CancelFunc
}

func New(c Config) *Engine {
	// значения по-умолчанию
	if c.MaxQueue == 0 {
		c.MaxQueue = 50
	}
	if c.DebounceSame == 0 {
		c.DebounceSame = 2 * time.Second
	}
	if c.MergeWindow == 0 {
		c.MergeWindow = 120 * time.Millisecond
	}
	if c.MinInterval == 0 {
		c.MinInterval = 300 * time.Millisecond
	}
	if c.Volume <= 0 || c.Volume > 1 {
		c.Volume = 0.8 // дефолт: 80 %
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
         PUBLIC API
------------------------------------------------------------------*/

func (e *Engine) Shutdown() {
	e.cancel()
	e.stopMelody()
}

// Speak – добавить фразу в очередь с приоритетом.
func (e *Engine) Speak(p Priority, text string) {
	if text == "" {
		return
	}

	// anti-spam: одинаковая фраза не чаще DebounceSame
	e.mu.Lock()
	if t, ok := e.lastSpoken[text]; ok && time.Since(t) < e.cfg.DebounceSame {
		e.mu.Unlock()
		return
	}
	e.lastSpoken[text] = time.Now()
	e.mu.Unlock()

	m := message{txt: text, pr: p, t: time.Now()}

	select {
	case e.queue <- m:
	default:
		// очередь заполнена
		if p <= High { // вытолкнём менее важное
			<-e.queue
			e.queue <- m
		}
	}
}

func (e *Engine) MelodyOn() {
	// если уже играет – ничего не делаем
	if e.melodyCmd != nil && e.melodyCmd.Process != nil && e.melodyCmd.ProcessState == nil {
		return
	}

	volArg := fmt.Sprintf("%.2f", e.cfg.Volume)

	// afplay:
	//   -q 1   → low-latency
	//   -v N   → громкость
	//   -l 0   → loop ∞
	e.melodyCmd = exec.CommandContext(
		e.ctx,
		"afplay",
		"/System/Library/Sounds/Submarine.aiff",
		"-q", "1",
		"-v", volArg,
		"-l", "0",
	)

	_ = e.melodyCmd.Start() // ошибки не критичны
}

func (e *Engine) MelodyOff() { e.stopMelody() }

/*------------------------------------------------------------------
         INTERNAL LOOP
------------------------------------------------------------------*/

func (e *Engine) runner() {
	for {
		select {
		case <-e.ctx.Done():
			return

		case first := <-e.queue:
			// собираем всё, что придёт за MergeWindow
			buf := []string{first.txt}
			timeout := time.NewTimer(e.cfg.MergeWindow)

		loop:
			for {
				select {
				case <-e.ctx.Done():
					timeout.Stop()
					return
				case next := <-e.queue:
					// если System / High – выходим раньше
					if next.pr <= High {
						buf = append(buf, next.txt)
						break loop
					}
					buf = append(buf, next.txt)
				case <-timeout.C:
					break loop
				}
			}

			// соблюдаем паузу minInterval
			if delta := time.Since(e.lastSay); delta < e.cfg.MinInterval {
				time.Sleep(e.cfg.MinInterval - delta)
			}

			e.pauseMelody()
			e.runSay(join(buf))
			e.resumeMelody()

			e.mu.Lock()
			e.lastSay = time.Now()
			e.mu.Unlock()
		}
	}
}

/*------------------------------------------------------------------
                     helpers
------------------------------------------------------------------*/

func join(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	default:
		return strings.Join(parts, ". ")
	}
}

func (e *Engine) runSay(text string) {
	args := []string{
		"-v", e.cfg.Voice,
		"-r", strconv.Itoa(e.cfg.Rate),
		text,
	}
	_ = exec.CommandContext(e.ctx, "say", args...).Run()
}

/*------------------------------------------------------------------
                      Melody Pause / Resume
------------------------------------------------------------------*/

func (e *Engine) pauseMelody() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		_ = e.melodyCmd.Process.Signal(syscall.SIGSTOP)
		time.Sleep(50 * time.Millisecond)
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
