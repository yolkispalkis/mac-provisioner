package configurator

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"mac-provisioner/internal/notification"
)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) RestoreDevice(identifier string, notifyMgr *notification.Manager) error {
	fmt.Printf("Starting restore for device %s...\n", identifier)

	// Определяем, это ECID или серийный номер
	var cmd *exec.Cmd
	if strings.HasPrefix(identifier, "0x") {
		// Это ECID, используем его для восстановления
		fmt.Printf("Using ECID for restore: %s\n", identifier)
		cmd = exec.Command("cfgutil", "restore",
			"-e", identifier,
			"--erase")
	} else {
		// Это серийный номер
		fmt.Printf("Using serial number for restore: %s\n", identifier)
		cmd = exec.Command("cfgutil", "restore",
			"-s", identifier,
			"--erase")
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cfgutil restore failed: %w", err)
	}

	return m.waitForRestoreCompletion(identifier, notifyMgr)
}

func (m *Manager) waitForRestoreCompletion(identifier string, notifyMgr *notification.Manager) error {
	fmt.Printf("Waiting for restore completion for device %s...\n", identifier)

	maxWaitTime := 30 * time.Minute
	checkInterval := 30 * time.Second
	timeout := time.After(maxWaitTime)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastStatus := ""

	for {
		select {
		case <-timeout:
			return fmt.Errorf("restore timeout for device %s", identifier)
		case <-ticker.C:
			status, err := m.getDeviceStatus(identifier)
			if err != nil {
				continue
			}

			fmt.Printf("Device %s status: %s\n", identifier, status)

			if status != lastStatus && status != "Device not found" {
				notifyMgr.RestoreProgress(identifier, m.getReadableStatus(status))
				lastStatus = status
			}

			if m.isRestoreComplete(status) {
				fmt.Printf("Restore completed for device %s\n", identifier)
				return nil
			}
		}
	}
}

func (m *Manager) getDeviceStatus(identifier string) (string, error) {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Ищем по ECID или серийному номеру
		if strings.Contains(line, identifier) {
			return line, nil
		}
	}

	return "Device not found", nil
}

func (m *Manager) getReadableStatus(status string) string {
	status = strings.ToLower(status)

	if strings.Contains(status, "dfu") {
		return "в режиме Д Ф У"
	}
	if strings.Contains(status, "recovery") {
		return "в режиме восстановления"
	}
	if strings.Contains(status, "restoring") {
		return "восстанавливает прошивку"
	}
	if strings.Contains(status, "available") {
		return "доступно"
	}
	if strings.Contains(status, "paired") {
		return "сопряжено и готово"
	}

	return "неизвестный статус"
}

func (m *Manager) isRestoreComplete(status string) bool {
	status = strings.ToLower(status)

	return !strings.Contains(status, "dfu") &&
		!strings.Contains(status, "recovery") &&
		!strings.Contains(status, "restoring") &&
		(strings.Contains(status, "available") || strings.Contains(status, "paired"))
}
