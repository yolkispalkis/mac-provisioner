package notifier

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type Notifier interface {
	Speak(text string)
	SpeakImmediately(text string)
	Announce(text string)
}

type VoiceNotifier struct {
	enabled        bool
	voice          string
	rate           int
	queue          chan string
	lastSpoken     map[string]time.Time
	mu             sync.Mutex
	announceCancel context.CancelFunc
	announceMu     sync.Mutex
}

func New(enabled bool, voice string, rate int) *VoiceNotifier {
	n := &VoiceNotifier{
		enabled:    enabled,
		voice:      voice,
		rate:       rate,
		queue:      make(chan string, 20),
		lastSpoken: make(map[string]time.Time),
	}

	if enabled {
		go n.processQueue()
	}

	return n
}

func (n *VoiceNotifier) Speak(text string) {
	if !n.enabled || text == "" {
		return
	}

	n.mu.Lock()
	if time.Since(n.lastSpoken[text]) < 10*time.Second {
		n.mu.Unlock()
		return
	}
	n.lastSpoken[text] = time.Now()
	n.mu.Unlock()

	select {
	case n.queue <- text:
	default:
		log.Println("[WARN] Очередь уведомлений переполнена, сообщение пропущено:", text)
	}
}

func (n *VoiceNotifier) SpeakImmediately(text string) {
	if !n.enabled || text == "" {
		return
	}
	go func() {
		args := []string{"-v", n.voice, "-r", strconv.Itoa(n.rate), text}
		cmd := exec.Command("say", args...)
		if err := cmd.Run(); err != nil {
			log.Printf("[WARN] Ошибка синтеза речи (немедленно): %v", err)
		}
	}()
}

func (n *VoiceNotifier) Announce(text string) {
	if !n.enabled || text == "" {
		return
	}

	n.announceMu.Lock()
	if n.announceCancel != nil {
		n.announceCancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	n.announceCancel = cancel
	n.announceMu.Unlock()

	go func() {
		args := []string{"-v", n.voice, "-r", strconv.Itoa(n.rate), text}
		cmd := exec.CommandContext(ctx, "say", args...)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != context.Canceled {
				log.Printf("[WARN] Ошибка анонса статуса: %v", err)
			}
		}
	}()
}

func (n *VoiceNotifier) processQueue() {
	for text := range n.queue {
		args := []string{"-v", n.voice, "-r", strconv.Itoa(n.rate), text}
		cmd := exec.Command("say", args...)
		if err := cmd.Run(); err != nil {
			log.Printf("[WARN] Ошибка синтеза речи: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
