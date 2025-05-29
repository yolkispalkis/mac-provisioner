package notification

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
)

type Manager struct {
	config           config.NotificationConfig
	lastNotification time.Time
	minInterval      time.Duration

	speechQueue chan string
	speechMutex sync.Mutex
	isPlaying   bool
}

/*──────────────────────────────────────────────────────────
  Конструктор
  ──────────────────────────────────────────────────────────*/

func New(cfg config.NotificationConfig) *Manager {
	m := &Manager{
		config:      cfg,
		minInterval: 2 * time.Second,
		speechQueue: make(chan string, 10),
	}
	go m.processSpeechQueue()
	return m
}

/*──────────────────────────────────────────────────────────
  Публичные методы-события
  ──────────────────────────────────────────────────────────*/

func (m *Manager) DeviceDetected(device interface{}) {
	if m.config.Enabled {
		m.speak("Обнаружено новое устройство. " + voiceName(device))
	}
}

func (m *Manager) DeviceConnected(device interface{}) {
	if m.config.Enabled && m.canNotify() {
		m.speak("Подключено устройство. " + voiceName(device))
	}
}

func (m *Manager) DeviceDisconnected(device interface{}) {
	if m.config.Enabled && m.canNotify() {
		m.speak("Отключено устройство. " + voiceName(device))
	}
}

func (m *Manager) EnteringDFUMode(device interface{}) {
	if m.config.Enabled {
		m.speak("Переход в режим восстановления для " + voiceName(device))
	}
}

func (m *Manager) DFUModeEntered(device interface{}) {
	if m.config.Enabled {
		m.speak(voiceName(device) + " перешло в режим восстановления. Готово к прошивке.")
	}
}

func (m *Manager) StartingRestore(device interface{}) {
	if m.config.Enabled {
		m.speak("Начинается прошивка устройства " + voiceName(device) + ".")
	}
}

func (m *Manager) RestoreProgress(device interface{}, status string) {
	if m.config.Enabled && m.canNotify() {
		m.speak(voiceName(device) + ". " + status)
	}
}

func (m *Manager) RestoreCompleted(device interface{}) {
	if m.config.Enabled {
		m.speak("Прошивка успешно завершена для " + voiceName(device))
	}
}

func (m *Manager) RestoreFailed(device interface{}, err string) {
	if m.config.Enabled {
		m.speak("Ошибка прошивки " + voiceName(device) + ". " + m.simplifyError(err))
	}
}

func (m *Manager) SystemStarted() {
	if m.config.Enabled {
		m.speak("Мак Провижнер запущен и готов к работе.")
	}
}

func (m *Manager) SystemShutdown() {
	if m.config.Enabled {
		m.speakImmediate("Мак Провижнер завершает работу. До свидания!")
	}
}

func (m *Manager) Error(errMsg string) {
	if m.config.Enabled {
		m.speak("Системная ошибка. " + m.simplifyError(errMsg))
	}
}

func (m *Manager) ManualDFURequired(device interface{}) {
	if m.config.Enabled {
		m.speak("Для " + voiceName(device) + " требуется ручной переход в режим восстановления.")
	}
}

func (m *Manager) WaitingForDFU(device interface{}) {
	if m.config.Enabled {
		m.speak("Ожидание перехода " + voiceName(device) + " в режим восстановления.")
	}
}

func (m *Manager) DeviceReady(device interface{}) {
	if m.config.Enabled && m.canNotify() {
		m.speak(voiceName(device) + " готово к работе.")
	}
}

/*──────────────────────────────────────────────────────────
  Реализация очереди TTS
  ──────────────────────────────────────────────────────────*/

func (m *Manager) speak(msg string) {
	select {
	case m.speechQueue <- msg:
	default:
		fmt.Printf("⚠️  Очередь голосовых сообщений переполнена, пропуск. %s\n", msg)
	}
}

func (m *Manager) speakImmediate(msg string) {
	m.speechMutex.Lock()
	defer m.speechMutex.Unlock()
	m.executeSpeech(msg)
}

func (m *Manager) processSpeechQueue() {
	for msg := range m.speechQueue {
		m.speechMutex.Lock()
		m.isPlaying = true

		m.executeSpeech(msg)

		m.isPlaying = false
		m.speechMutex.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}

func (m *Manager) executeSpeech(msg string) {
	if m.config.Volume != 1.0 {
		_ = exec.Command("osascript", "-e",
			fmt.Sprintf("set volume output volume %d", int(m.config.Volume*100))).Run()
	}
	args := []string{"-v", m.config.Voice, "-r", strconv.Itoa(m.config.Rate), msg}
	if err := exec.Command("say", args...).Run(); err != nil {
		fmt.Printf("⚠️  say error: %v\n", err)
	}
}

/*──────────────────────────────────────────────────────────
  Хелперы
  ──────────────────────────────────────────────────────────*/

func (m *Manager) canNotify() bool {
	now := time.Now()
	if now.Sub(m.lastNotification) < m.minInterval {
		return false
	}
	m.lastNotification = now
	return true
}

func voiceName(obj interface{}) string {
	// Предпочитаем «читабельную модель» без ECID
	if d, ok := obj.(interface{ GetReadableModel() string }); ok {
		return d.GetReadableModel()
	}
	if d, ok := obj.(interface{ GetFriendlyName() string }); ok {
		return d.GetFriendlyName()
	}
	return "устройство"
}

func (m *Manager) simplifyError(err string) string {
	l := strings.ToLower(err)
	switch {
	case strings.Contains(l, "timeout"):
		return "превышено время ожидания"
	case strings.Contains(l, "dfu"):
		return "ошибка режима восстановления"
	case strings.Contains(l, "restore"):
		return "ошибка процесса восстановления"
	case strings.Contains(l, "connect"):
		return "ошибка подключения"
	case strings.Contains(l, "permission"), strings.Contains(l, "access"):
		return "ошибка доступа"
	}
	words := strings.Fields(err)
	if len(words) > 5 {
		return strings.Join(words[:5], " ")
	}
	return err
}

/*──────────────────────────────────────────────────────────
  Разное / утилиты
  ──────────────────────────────────────────────────────────*/

func (m *Manager) IsPlaying() bool {
	m.speechMutex.Lock()
	defer m.speechMutex.Unlock()
	return m.isPlaying
}

func (m *Manager) ClearQueue() {
	for {
		select {
		case <-m.speechQueue:
		default:
			return
		}
	}
}

func (m *Manager) StopAll() {
	_ = exec.Command("killall", "say").Run()
	m.ClearQueue()
}

func (m *Manager) PlayAlert() {
	if m.config.Enabled {
		go exec.Command("afplay", "/System/Library/Sounds/Sosumi.aiff").Run()
	}
}

func (m *Manager) PlaySuccess() {
	if m.config.Enabled {
		go exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Run()
	}
}

func (m *Manager) TestVoice() {
	m.speakImmediate("Тест голоса Мак Провижнер.")
}

func (m *Manager) GetAvailableVoices() ([]string, error) {
	out, err := exec.Command("say", "-v", "?").Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	var voices []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		parts := strings.Fields(l)
		if len(parts) > 0 {
			voices = append(voices, parts[0])
		}
	}
	return voices, nil
}
