package notification

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"mac-provisioner/internal/config"
)

type Manager struct {
	config           config.NotificationConfig
	lastNotification time.Time
	minInterval      time.Duration
}

func New(cfg config.NotificationConfig) *Manager {
	return &Manager{
		config:      cfg,
		minInterval: 2 * time.Second,
	}
}

func (m *Manager) DeviceDetected(serialNumber, model string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Обнаружено новое устройство. %s с серийным номером %s",
		model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DeviceConnected(serialNumber, model string) {
	if !m.config.Enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("Подключено устройство: %s %s",
		model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DeviceDisconnected(serialNumber, model string) {
	if !m.config.Enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("Отключено устройство: %s %s",
		model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) EnteringDFUMode(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Переход в режим восстановления для устройства %s",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DFUModeEntered(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Устройство %s перешло в режим восстановления. Готово к прошивке.",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) StartingRestore(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Начинается восстановление прошивки для устройства %s. Процесс может занять несколько минут.",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) RestoreProgress(serialNumber, status string) {
	if !m.config.Enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("Устройство %s: %s",
		m.formatSerialNumber(serialNumber), status)
	m.speak(message)
}

func (m *Manager) RestoreCompleted(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Отлично! Восстановление успешно завершено для устройства %s. Устройство готово к использованию.",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) RestoreFailed(serialNumber, error string) {
	if !m.config.Enabled {
		return
	}

	simplifiedError := m.simplifyError(error)
	message := fmt.Sprintf("Внимание! Восстановление не удалось для устройства %s. %s",
		m.formatSerialNumber(serialNumber), simplifiedError)
	m.speak(message)
}

func (m *Manager) SystemStarted() {
	if !m.config.Enabled {
		return
	}

	message := "Мак Провижнер успешно запущен. Начинается мониторинг подключенных устройств."
	m.speak(message)
}

func (m *Manager) SystemShutdown() {
	if !m.config.Enabled {
		return
	}

	message := "Мак Провижнер завершает работу. До свидания!"
	m.speak(message)
}

func (m *Manager) Error(errorMsg string) {
	if !m.config.Enabled {
		return
	}

	simplifiedError := m.simplifyError(errorMsg)
	message := fmt.Sprintf("Системная ошибка: %s", simplifiedError)
	m.speak(message)
}

func (m *Manager) ManualDFURequired(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Для устройства %s требуется ручной переход в режим восстановления. Проверьте консоль для получения подробных инструкций.",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) WaitingForDFU(serialNumber string) {
	if !m.config.Enabled {
		return
	}

	message := fmt.Sprintf("Ожидание перехода устройства %s в режим восстановления. Следуйте инструкциям на экране.",
		m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) speak(message string) {
	go func() {
		args := []string{
			"-v", m.config.Voice,
			"-r", strconv.Itoa(m.config.Rate),
		}

		// Устанавливаем громкость
		if m.config.Volume != 1.0 {
			volumeCmd := exec.Command("osascript", "-e",
				fmt.Sprintf("set volume output volume %d", int(m.config.Volume*100)))
			volumeCmd.Run()
		}

		args = append(args, message)
		cmd := exec.Command("say", args...)
		cmd.Run()
	}()
}

func (m *Manager) formatSerialNumber(serialNumber string) string {
	// Убираем префикс DFU- если есть
	if strings.HasPrefix(serialNumber, "DFU-") {
		serialNumber = strings.TrimPrefix(serialNumber, "DFU-")
	}

	if len(serialNumber) > 6 {
		var formatted strings.Builder
		for i, char := range serialNumber {
			if i > 0 {
				formatted.WriteString(" ")
			}
			formatted.WriteRune(char)
		}
		return formatted.String()
	}
	return serialNumber
}

func (m *Manager) simplifyError(error string) string {
	error = strings.ToLower(error)

	if strings.Contains(error, "timeout") {
		return "Превышено время ожидания"
	}
	if strings.Contains(error, "dfu") {
		return "Ошибка режима восстановления"
	}
	if strings.Contains(error, "restore") {
		return "Ошибка процесса восстановления"
	}
	if strings.Contains(error, "connection") || strings.Contains(error, "connect") {
		return "Ошибка подключения"
	}
	if strings.Contains(error, "permission") || strings.Contains(error, "access") {
		return "Ошибка доступа"
	}

	words := strings.Fields(error)
	if len(words) > 5 {
		return strings.Join(words[:5], " ")
	}

	return error
}

func (m *Manager) canNotify() bool {
	now := time.Now()
	if now.Sub(m.lastNotification) < m.minInterval {
		return false
	}
	m.lastNotification = now
	return true
}

func (m *Manager) PlayAlert() {
	if !m.config.Enabled {
		return
	}

	go func() {
		cmd := exec.Command("afplay", "/System/Library/Sounds/Sosumi.aiff")
		cmd.Run()
	}()
}

func (m *Manager) PlaySuccess() {
	if !m.config.Enabled {
		return
	}

	go func() {
		cmd := exec.Command("afplay", "/System/Library/Sounds/Glass.aiff")
		cmd.Run()
	}()
}

func (m *Manager) TestVoice() {
	message := "Тест голоса Мак Провижнер. Так будут звучать уведомления с текущими настройками."
	m.speak(message)
}

func (m *Manager) GetAvailableVoices() ([]string, error) {
	cmd := exec.Command("say", "-v", "?")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(output), "\n")
	var voices []string

	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			parts := strings.Fields(line)
			if len(parts) > 0 {
				voices = append(voices, parts[0])
			}
		}
	}

	return voices, nil
}
