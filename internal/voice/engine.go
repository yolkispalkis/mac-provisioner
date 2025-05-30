package voice

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

/*
Использование:

	v := voice.New(voice.Config{Voice:"Milena", Rate:200})
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
	lastSpoken map[string]time.Time // anti-spam
	lastSay    time.Time            // когда запустился предыдущий say

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
			// Собираем всё, что придёт за MergeWindow
			buf := []string{first.txt}
			timeout := time.NewTimer(e.cfg.MergeWindow)

		loop:
			for {
				select {
				case <-e.ctx.Done():
					timeout.Stop()
					return
				case next := <-e.queue:
					// если System / High – прерываем «приём» и говорим немедленно
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
		// короткая пауза между склеенными фразами
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
