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
Пример использования:

        v := voice.New(voice.Config{
                Voice:  "Milena",
                Rate:   200,
                Volume: 0.8, // 0.0‒1.0, влияет на громкость мелодии
        })

        v.Speak(voice.Normal, "Начинается прошивка")
        v.MelodyOn()      // фоновая мелодия
        …                 // работа
        v.MelodyOff()     // остановка
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
	Volume       float64       // 0.0…1.0 — используется в MelodyOn
	MaxQueue     int           // ёмкость канала
	DebounceSame time.Duration // анти-спам одинаковых фраз
	MergeWindow  time.Duration // «склейка» сообщений < MergeWindow
	MinInterval  time.Duration // пауза между say-процессами
}

// внутренний элемент очереди
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
		c.Volume = 0.8 // дефолт 80 %
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

// Speak — добавить фразу в очередь с заданным приоритетом.
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

/*
MelodyOn запускает бесконечный afplay-луп (Submarine/Hero);
если мелодия уже играет — повторный вызов игнорируется.
*/
func (e *Engine) MelodyOn() {
	// уже играет?
	if e.melodyCmd != nil &&
		e.melodyCmd.Process != nil &&
		e.melodyCmd.ProcessState == nil {
		return
	}

	// основной и резервный звук macOS
	sound := "/System/Library/Sounds/Submarine.aiff"
	if _, err := os.Stat(sound); err != nil {
		sound = "/System/Library/Sounds/Hero.aiff"
	}

	volArg := fmt.Sprintf("%.2f", e.cfg.Volume)

	e.melodyCmd = exec.CommandContext(
		e.ctx,
		"afplay",
		sound,
		"-q", "1", // low-latency
		"-v", volArg,
		"-l", "0", // loop ∞
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
					// System / High — прекращаем приём и говорим немедленно
					if next.pr <= High {
						buf = append(buf, next.txt)
						break loop
					}
					buf = append(buf, next.txt)
				case <-timeout.C:
					break loop
				}
			}

			// пауза minInterval
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
		time.Sleep(50 * time.Millisecond) // чтобы уверенно приостановить звук
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
