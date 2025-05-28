package notification

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Manager struct {
	enabled          bool
	voice            string
	volume           float64
	rate             int
	lastNotification time.Time
	minInterval      time.Duration
}

func NewManager() *Manager {
	return &Manager{
		enabled:     true,
		voice:       "Alex",
		volume:      0.7,
		rate:        200,
		minInterval: 2 * time.Second,
	}
}

func (m *Manager) SetEnabled(enabled bool) {
	m.enabled = enabled
}

func (m *Manager) SetVoice(voice string) {
	m.voice = voice
}

func (m *Manager) SetVolume(volume float64) {
	if volume >= 0.0 && volume <= 1.0 {
		m.volume = volume
	}
}

func (m *Manager) SetRate(rate int) {
	if rate > 0 {
		m.rate = rate
	}
}

func (m *Manager) DeviceDetected(serialNumber, model string) {
	if !m.enabled {
		return
	}

	message := fmt.Sprintf("Device detected. %s with serial number %s", model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DeviceConnected(serialNumber, model string) {
	if !m.enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("New device connected: %s %s", model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DeviceDisconnected(serialNumber, model string) {
	if !m.enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("Device disconnected: %s %s", model, m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) EnteringDFUMode(serialNumber string) {
	if !m.enabled {
		return
	}

	message := fmt.Sprintf("Entering D F U mode for device %s", m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) DFUModeEntered(serialNumber string) {
	if !m.enabled {
		return
	}

	message := fmt.Sprintf("Device %s is now in D F U mode. Ready for restore.", m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) StartingRestore(serialNumber string) {
	if !m.enabled {
		return
	}

	message := fmt.Sprintf("Starting firmware restore for device %s. This may take several minutes.", m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) RestoreProgress(serialNumber, status string) {
	if !m.enabled || !m.canNotify() {
		return
	}

	message := fmt.Sprintf("Device %s is %s", m.formatSerialNumber(serialNumber), status)
	m.speak(message)
}

func (m *Manager) RestoreCompleted(serialNumber string) {
	if !m.enabled {
		return
	}

	message := fmt.Sprintf("Great! Restore completed successfully for device %s. Device is ready for use.", m.formatSerialNumber(serialNumber))
	m.speak(message)
}

func (m *Manager) RestoreFailed(serialNumber, error string) {
	if !m.enabled {
		return
	}

	simplifiedError := m.simplifyError(error)
	message := fmt.Sprintf("Alert! Restore failed for device %s. %s", m.formatSerialNumber(serialNumber), simplifiedError)
	m.speak(message)
}

func (m *Manager) SystemStarted() {
	if !m.enabled {
		return
	}

	message := "Mac Provisioner has started successfully. Now monitoring for connected devices."
	m.speak(message)
}

func (m *Manager) SystemShutdown() {
	if !m.enabled {
		return
	}

	message := "Mac Provisioner is shutting down. Goodbye!"
	m.speak(message)
}

func (m *Manager) Error(errorMsg string) {
	if !m.enabled {
		return
	}

	simplifiedError := m.simplifyError(errorMsg)
	message := fmt.Sprintf("System error: %s", simplifiedError)
	m.speak(message)
}

func (m *Manager) RealTimeMonitoringStarted() {
	if !m.enabled {
		return
	}

	message := "Real-time device monitoring activated. Devices will be detected instantly upon connection."
	m.speak(message)
}

func (m *Manager) speak(message string) {
	go func() {
		args := []string{
			"-v", m.voice,
			"-r", strconv.Itoa(m.rate),
		}

		if m.volume != 1.0 {
			volumeCmd := exec.Command("osascript", "-e",
				fmt.Sprintf("set volume output volume %d", int(m.volume*100)))
			volumeCmd.Run()
		}

		args = append(args, message)
		cmd := exec.Command("say", args...)
		cmd.Run()
	}()
}

func (m *Manager) formatSerialNumber(serialNumber string) string {
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
		return "Operation timed out"
	}
	if strings.Contains(error, "dfu") {
		return "D F U mode error"
	}
	if strings.Contains(error, "restore") {
		return "Restore process error"
	}
	if strings.Contains(error, "connection") || strings.Contains(error, "connect") {
		return "Connection error"
	}
	if strings.Contains(error, "permission") || strings.Contains(error, "access") {
		return "Permission error"
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

func (m *Manager) TestVoice() {
	message := "Mac Provisioner voice test. This is how notifications will sound with current settings."
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

func (m *Manager) PlayAlert() {
	if !m.enabled {
		return
	}

	go func() {
		cmd := exec.Command("afplay", "/System/Library/Sounds/Sosumi.aiff")
		cmd.Run()
	}()
}

func (m *Manager) PlaySuccess() {
	if !m.enabled {
		return
	}

	go func() {
		cmd := exec.Command("afplay", "/System/Library/Sounds/Glass.aiff")
		cmd.Run()
	}()
}
