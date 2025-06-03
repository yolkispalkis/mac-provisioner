package voice

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mac-provisioner/internal/config"
)

type Priority int

const (
	System Priority = iota
	High
	Normal
	Low
)

type Message struct {
	Text     string
	Priority Priority
	Time     time.Time
}

type Engine struct {
	config     config.NotificationConfig
	queue      chan Message
	ctx        context.Context
	cancel     context.CancelFunc
	melodyCmd  *exec.Cmd
	lastSpoken map[string]time.Time
	mutex      sync.Mutex
}

func New(cfg config.NotificationConfig) *Engine {
	ctx, cancel := context.WithCancel(context.Background())

	engine := &Engine{
		config:     cfg,
		queue:      make(chan Message, 50),
		ctx:        ctx,
		cancel:     cancel,
		lastSpoken: make(map[string]time.Time),
	}

	go engine.processQueue()
	return engine
}

func (e *Engine) Shutdown() {
	e.cancel()
	e.stopMelody()
}

func (e *Engine) Speak(priority Priority, text string) {
	if !e.config.Enabled || text == "" {
		return
	}

	// Защита от спама одинаковых сообщений
	e.mutex.Lock()
	if lastTime, exists := e.lastSpoken[text]; exists {
		if time.Since(lastTime) < 2*time.Second {
			e.mutex.Unlock()
			return
		}
	}
	e.lastSpoken[text] = time.Now()
	e.mutex.Unlock()

	message := Message{
		Text:     text,
		Priority: priority,
		Time:     time.Now(),
	}

	select {
	case e.queue <- message:
	default:
		// Очередь заполнена, пропускаем сообщения с низким приоритетом
		if priority <= High {
			// Удаляем одно сообщение и добавляем новое
			select {
			case <-e.queue:
			default:
			}
			e.queue <- message
		}
	}
}

func (e *Engine) MelodyOn() {
	if e.melodyCmd != nil && e.melodyCmd.Process != nil {
		return // Мелодия уже играет
	}

	soundFile := "/System/Library/Sounds/Submarine.aiff"
	volumeArg := fmt.Sprintf("%.2f", e.config.Volume)

	// Создаем команду для циклического воспроизведения
	loopCommand := fmt.Sprintf("while true; do afplay -v %s %s; sleep 0.1; done", volumeArg, soundFile)
	e.melodyCmd = exec.CommandContext(e.ctx, "sh", "-c", loopCommand)

	if err := e.melodyCmd.Start(); err != nil {
		log.Printf("⚠️ Не удалось запустить фоновую мелодию: %v", err)
		e.melodyCmd = nil
	}
}

func (e *Engine) MelodyOff() {
	e.stopMelody()
}

func (e *Engine) processQueue() {
	for {
		select {
		case <-e.ctx.Done():
			return
		case message := <-e.queue:
			e.speakMessage(message)
		}
	}
}

func (e *Engine) speakMessage(message Message) {
	// Приостанавливаем мелодию на время речи
	e.pauseMelody()
	defer e.resumeMelody()

	// Выполняем синтез речи
	args := []string{
		"-v", e.config.Voice,
		"-r", strconv.Itoa(e.config.Rate),
		message.Text,
	}

	cmd := exec.CommandContext(e.ctx, "say", args...)
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ Ошибка синтеза речи: %v", err)
	}

	// Небольшая пауза между сообщениями
	time.Sleep(300 * time.Millisecond)
}

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
		_ = e.melodyCmd.Wait()
		e.melodyCmd = nil
	}
}
