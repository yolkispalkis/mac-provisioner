package dfu

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type Manager struct{}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) EnterDFUMode(serialNumber string) error {
	cmd := exec.Command("macvdmtool", "reboot", "-s", serialNumber, "--dfu")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute macvdmtool: %w", err)
	}

	time.Sleep(10 * time.Second)
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) waitForDFUMode(serialNumber string) error {
	maxAttempts := 30

	for i := 0; i < maxAttempts; i++ {
		if m.isInDFUMode(serialNumber) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("device %s did not enter DFU mode within timeout", serialNumber)
}

func (m *Manager) isInDFUMode(serialNumber string) bool {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	outputStr := string(output)
	lines := strings.Split(outputStr, "\n")

	for _, line := range lines {
		if strings.Contains(line, serialNumber) {
			state := strings.ToLower(line)
			return strings.Contains(state, "dfu") || strings.Contains(state, "recovery")
		}
	}

	return false
}
