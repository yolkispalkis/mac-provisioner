package notification

import (
	"os/exec"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/device"
	"mac-provisioner/internal/voice"
)

type Manager struct {
	cfg             config.NotificationConfig
	voice           *voice.Engine
	lastLowPriority time.Time
	minInterval     time.Duration
}

/*─────────────────────────────────────────────────────────
	КОНСТРУКТОР
─────────────────────────────────────────────────────────*/

func New(c config.NotificationConfig, v *voice.Engine) *Manager {
	return &Manager{
		cfg:         c,
		voice:       v,
		minInterval: 2 * time.Second,
	}
}

/*─────────────────────────────────────────────────────────
	         PUBLIC EVENTS  →  voice.Engine
─────────────────────────────────────────────────────────*/

func (m *Manager) DeviceDetected(d *device.Device) {
	m.sp(voice.High, "Обнаружено "+d.GetReadableModel())
}
func (m *Manager) DeviceConnected(d *device.Device) {
	if m.debounceLow() {
		m.sp(voice.Normal, "Подключено "+d.GetReadableModel())
	}
}
func (m *Manager) DeviceDisconnected(d *device.Device) {
	if m.debounceLow() {
		m.sp(voice.Normal, "Отключено "+d.GetReadableModel())
	}
}
func (m *Manager) EnteringDFUMode(d *device.Device) {
	m.sp(voice.High, "Переход в режим восстановления для "+d.GetReadableModel())
}
func (m *Manager) DFUModeEntered(d *device.Device) {
	m.sp(voice.High, d.GetReadableModel()+" в режиме восстановления. Готово к прошивке.")
}
func (m *Manager) StartingRestore(d *device.Device) {
	m.sp(voice.High, "Начинается прошивка "+d.GetReadableModel())
}
func (m *Manager) RestoreProgress(d *device.Device, status string) {
	if m.debounceLow() {
		m.sp(voice.Low, d.GetReadableModel()+". "+status)
	}
}
func (m *Manager) RestoreCompleted(d *device.Device) {
	m.sp(voice.High, "Прошивка завершена для "+d.GetReadableModel())
}
func (m *Manager) RestoreFailed(d *device.Device, err string) {
	m.sp(voice.High, "Ошибка прошивки "+d.GetReadableModel()+". "+err)
}
func (m *Manager) SystemStarted() {
	m.sp(voice.System, "Мак Провижнер запущен и готов к работе.")
}
func (m *Manager) SystemShutdown() {
	m.sp(voice.System, "Мак Провижнер завершает работу. До свидания!")
}
func (m *Manager) Error(msg string) {
	m.sp(voice.System, "Системная ошибка. "+msg)
}
func (m *Manager) ManualDFURequired(d *device.Device) {
	m.sp(voice.High, "Для "+d.GetReadableModel()+" нужен ручной DFU.")
}
func (m *Manager) WaitingForDFU(d *device.Device) {
	m.sp(voice.Normal, "Ожидается DFU от "+d.GetReadableModel())
}
func (m *Manager) DeviceReady(d *device.Device) {
	if m.debounceLow() {
		m.sp(voice.Normal, d.GetReadableModel()+" готов к работе.")
	}
}

/*─────────────────────────────────────────────────────────
	             ХЕЛПЕРЫ
─────────────────────────────────────────────────────────*/

func (m *Manager) sp(pr voice.Priority, txt string) {
	if !m.cfg.Enabled {
		return
	}
	m.voice.Speak(pr, txt)
}

func (m *Manager) debounceLow() bool {
	now := time.Now()
	if now.Sub(m.lastLowPriority) < m.minInterval {
		return false
	}
	m.lastLowPriority = now
	return true
}

/*─────────────────────────────────────────────────────────
	         ALERT / SUCCESS  (звук-колокольчики)
─────────────────────────────────────────────────────────*/

func (m *Manager) PlayAlert() {
	if m.cfg.Enabled {
		go exec.Command("afplay", "/System/Library/Sounds/Sosumi.aiff").Run()
	}
}
func (m *Manager) PlaySuccess() {
	if m.cfg.Enabled {
		go exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Run()
	}
}
