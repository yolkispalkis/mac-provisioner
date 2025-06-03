package notification

import (
	"log"
	"os/exec"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/device"
	"mac-provisioner/internal/voice"
)

type Manager struct {
	config      config.NotificationConfig
	voice       *voice.Engine
	lastNotify  time.Time
	minInterval time.Duration
}

func New(cfg config.NotificationConfig, voiceEngine *voice.Engine) *Manager {
	return &Manager{
		config:      cfg,
		voice:       voiceEngine,
		minInterval: 2 * time.Second,
	}
}

func (m *Manager) SystemStarted() {
	m.speak(voice.System, "Мак Провижнер запущен и готов к работе")
}

func (m *Manager) SystemShutdown() {
	m.speak(voice.System, "Мак Провижнер завершает работу")
}

func (m *Manager) DeviceConnected(dev *device.Device) {
	if m.shouldNotify() {
		m.playSound("Pop")
		m.speak(voice.Normal, "Подключено "+dev.GetReadableName())
	}
}

func (m *Manager) DeviceDisconnected(dev *device.Device) {
	if m.shouldNotify() {
		m.speak(voice.Normal, "Отключено "+dev.GetReadableName())
	}
}

func (m *Manager) EnteringDFUMode(dev *device.Device) {
	m.speak(voice.High, "Переход в режим восстановления для "+dev.GetReadableName())
}

func (m *Manager) DFUModeEntered(dev *device.Device) {
	m.playSound("Glass")
	m.speak(voice.High, dev.GetReadableName()+" в режиме восстановления. Готово к прошивке")
}

func (m *Manager) StartingRestore(dev *device.Device) {
	m.speak(voice.High, "Начинается прошивка "+dev.GetReadableName())
}

func (m *Manager) RestoreProgress(dev *device.Device, status string) {
	if m.shouldNotify() {
		m.speak(voice.Low, dev.GetReadableName()+". "+status)
	}
}

func (m *Manager) RestoreCompleted(dev *device.Device) {
	m.playSound("Glass")
	m.speak(voice.High, "Прошивка завершена для "+dev.GetReadableName()+". Устройство добавлено в период охлаждения")
}

func (m *Manager) RestoreFailed(dev *device.Device, reason string) {
	m.playSound("Sosumi")
	m.speak(voice.High, "Ошибка прошивки "+dev.GetReadableName()+". "+reason)
}

func (m *Manager) ManualDFURequired(dev *device.Device) {
	m.playSound("Sosumi")
	m.speak(voice.High, "Для "+dev.GetReadableName()+" требуется ручной переход в DFU режим")
}

func (m *Manager) Error(message string) {
	m.playSound("Sosumi")
	m.speak(voice.System, "Системная ошибка. "+message)
}

func (m *Manager) speak(priority voice.Priority, text string) {
	if !m.config.Enabled {
		return
	}

	log.Printf("[Уведомление] %s", text)
	m.voice.Speak(priority, text)
}

func (m *Manager) shouldNotify() bool {
	now := time.Now()
	if now.Sub(m.lastNotify) < m.minInterval {
		return false
	}
	m.lastNotify = now
	return true
}

func (m *Manager) playSound(soundName string) {
	if !m.config.Enabled {
		return
	}

	soundPath := "/System/Library/Sounds/" + soundName + ".aiff"
	go exec.Command("afplay", soundPath).Run()
}
