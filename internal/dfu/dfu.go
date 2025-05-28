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
	// Сначала пробуем macvdmtool
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(serialNumber)
	}

	// Если macvdmtool недоступен, пробуем cfgutil
	return m.enterDFUWithCfgutil(serialNumber)
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(serialNumber string) error {
	cmd := exec.Command("macvdmtool", "reboot", "-s", serialNumber, "--dfu")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute macvdmtool: %w", err)
	}

	time.Sleep(10 * time.Second)
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) enterDFUWithCfgutil(serialNumber string) error {
	// Пробуем перевести в DFU режим через cfgutil
	cmd := exec.Command("cfgutil", "reboot", "-s", serialNumber, "--dfu")

	if err := cmd.Run(); err != nil {
		// Если cfgutil не поддерживает --dfu, пробуем другой способ
		return m.enterDFUManually(serialNumber)
	}

	time.Sleep(10 * time.Second)
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) enterDFUManually(serialNumber string) error {
	return fmt.Errorf("automatic DFU mode entry not supported for device %s. Please enter DFU mode manually:\n"+
		"1. Hold power button for 10 seconds to turn off\n"+
		"2. Press and hold power button for 3 seconds\n"+
		"3. While holding power, press and hold volume down for 10 seconds\n"+
		"4. Release power button but keep holding volume down for 5 more seconds", serialNumber)
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
