package notifier

import (
	"log"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Notifier - интерфейс для отправки уведомлений.
type Notifier interface {
	Speak(text string)
	SpeakImmediately(text string) // Новая функция для важных сообщений
}

// VoiceNotifier реализует Notifier с помощью системной команды 'say'.
type VoiceNotifier struct {
	enabled    bool
	voice      string
	rate       int
	queue      chan string
	lastSpoken map[string]time.Time
	mu         sync.Mutex
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

// Speak добавляет сообщение в очередь с защитой от спама.
func (n *VoiceNotifier) Speak(text string) {
	if !n.enabled || text == "" {
		return
	}

	n.mu.Lock()
	// Защита от спама одинаковыми сообщениями
	if time.Since(n.lastSpoken[text]) < 10*time.Second { // Увеличим интервал для обычных сообщений
		n.mu.Unlock()
		return
	}
	n.lastSpoken[text] = time.Now()
	n.mu.Unlock()

	select {
	case n.queue <- text:
	default:
		log.Println("⚠️ Очередь уведомлений переполнена, сообщение пропущено:", text)
	}
}

// SpeakImmediately озвучивает текст немедленно в отдельной горутине.
// Используется для критически важных, уникальных сообщений.
func (n *VoiceNotifier) SpeakImmediately(text string) {
	if !n.enabled || text == "" {
		return
	}
	go func() {
		args := []string{"-v", n.voice, "-r", strconv.Itoa(n.rate), text}
		cmd := exec.Command("say", args...)
		if err := cmd.Run(); err != nil {
			log.Printf("⚠️ Ошибка синтеза речи (немедленно): %v", err)
		}
	}()
}

func (n *VoiceNotifier) processQueue() {
	for text := range n.queue {
		args := []string{"-v", n.voice, "-r", strconv.Itoa(n.rate), text}
		cmd := exec.Command("say", args...)
		if err := cmd.Run(); err != nil {
			log.Printf("⚠️ Ошибка синтеза речи: %v", err)
		}
		// Небольшая пауза, чтобы сообщения не сливались
		time.Sleep(200 * time.Millisecond)
	}
}
