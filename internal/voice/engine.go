package voice

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

/*
Пример:

        v := voice.New(voice.Config{
                Voice:  "Milena",
                Rate:   200,
                Volume: 0.8,
        })

        v.MelodyOn()
        …
        v.MelodyOff()
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
	Volume       float64       // громкость мелодии 0.0…1.0
	MaxQueue     int           // ёмкость канала
	DebounceSame time.Duration // анти-спам одинаковых фраз
	MergeWindow  time.Duration // «склейка» сообщений
	MinInterval  time.Duration // пауза между say
}

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
		c.Volume = 0.8
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

func (e *Engine) Speak(p Priority, text string) {
	if text == "" {
		return
	}

	e.mu.Lock()
	if t, ok := e.lastSpoken[text]; ok && time.Since(t) < e.cfg.DebounceSame {
		e.mu.Unlock()
		return
	}
	e.lastSpoken[text] = time.Now()
	e.mu.Unlock()

	msg := message{txt: text, pr: p, t: time.Now()}

	select {
	case e.queue <- msg:
	default:
		if p <= High { // вытесняем менее важное
			<-e.queue
			e.queue <- msg
		}
	}
}

/*
MelodyOn — бесконечный зацикленный звук.
Если уже играет — повторный вызов игнорируется.
*/
func (e *Engine) MelodyOn() {
	if e.melodyCmd != nil &&
		e.melodyCmd.Process != nil &&
		e.melodyCmd.ProcessState == nil {
		return
	}

	sound := "/System/Library/Sounds/Submarine.aiff"
	if _, err := os.Stat(sound); err != nil {
		sound = "/System/Library/Sounds/Hero.aiff" // запасной
	}

	volArg := fmt.Sprintf("%.2f", e.cfg.Volume)

	e.melodyCmd = exec.CommandContext(
		e.ctx,
		"afplay",
		"-q", "1",
		"-v", volArg,
		"-l", "0", // ∞ loop
		sound, // ← ФАЙЛ ДОЛЖЕН БЫТЬ ПОСЛЕ ОПЦИЙ
	)

	if err := e.melodyCmd.Start(); err != nil {
		log.Printf("⚠️  Не удалось запустить мелодию: %v", err)
		e.melodyCmd = nil
	}
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
			buf := []string{first.txt}
			timeout := time.NewTimer(e.cfg.MergeWindow)

		collect:
			for {
				select {
				case <-e.ctx.Done():
					timeout.Stop()
					return
				case nxt := <-e.queue:
					if nxt.pr <= High {
						buf = append(buf, nxt.txt)
						break collect
					}
					buf = append(buf, nxt.txt)
				case <-timeout.C:
					break collect
				}
			}

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
