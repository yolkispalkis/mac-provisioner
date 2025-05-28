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

func (m *Manager) RestoreDevice(serialNumber string, notifyMgr *notification.Manager) error {
	fmt.Printf("Starting restore for device %s...\n", serialNumber)

	cmd := exec.Command("cfgutil", "restore",
		"-s", serialNumber,
		"--erase")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cfgutil restore failed: %w", err)
	}

	return m.waitForRestoreCompletion(serialNumber, notifyMgr)
}

func (m *Manager) waitForRestoreCompletion(serialNumber string, notifyMgr *notification.Manager) error {
	fmt.Printf("Waiting for restore completion for device %s...\n", serialNumber)

	maxWaitTime := 30 * time.Minute
	checkInterval := 30 * time.Second
	timeout := time.After(maxWaitTime)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastStatus := ""

	for {
		select {
		case <-timeout:
			return fmt.Errorf("restore timeout for device %s", serialNumber)
		case <-ticker.C:
			status, err := m.getDeviceStatus(serialNumber)
			if err != nil {
				continue
			}

			fmt.Printf("Device %s status: %s\n", serialNumber, status)

			if status != lastStatus && status != "Device not found" {
				notifyMgr.RestoreProgress(serialNumber, m.getReadableStatus(status))
				lastStatus = status
			}

			if m.isRestoreComplete(status) {
				fmt.Printf("Restore completed for device %s\n", serialNumber)
				return nil
			}
		}
	}
}

func (m *Manager) getDeviceStatus(serialNumber string) (string, error) {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, serialNumber) {
			return line, nil
		}
	}

	return "Device not found", nil
}

func (m *Manager) getReadableStatus(status string) string {
	status = strings.ToLower(status)

	if strings.Contains(status, "dfu") {
		return "in D F U mode"
	}
	if strings.Contains(status, "recovery") {
		return "in recovery mode"
	}
	if strings.Contains(status, "restoring") {
		return "restoring firmware"
	}
	if strings.Contains(status, "available") {
		return "available"
	}
	if strings.Contains(status, "paired") {
		return "paired and ready"
	}

	return "unknown status"
}

func (m *Manager) isRestoreComplete(status string) bool {
	status = strings.ToLower(status)

	return !strings.Contains(status, "dfu") &&
		!strings.Contains(status, "recovery") &&
		!strings.Contains(status, "restoring") &&
		(strings.Contains(status, "available") || strings.Contains(status, "paired"))
}
