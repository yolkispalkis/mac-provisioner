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

	// Если macvdmtool недоступен, используем cfgutil для перезагрузки и инструкции
	return m.enterDFUWithCfgutil(serialNumber)
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(serialNumber string) error {
	// macvdmtool работает с подключенным устройством через USB
	fmt.Printf("Using macvdmtool to enter DFU mode for device %s\n", serialNumber)

	cmd := exec.Command("macvdmtool", "dfu")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute macvdmtool dfu: %w", err)
	}

	fmt.Printf("macvdmtool dfu command executed successfully\n")
	time.Sleep(15 * time.Second) // Даем больше времени для перехода в DFU
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) enterDFUWithCfgutil(serialNumber string) error {
	fmt.Printf("macvdmtool not available, using cfgutil for device %s\n", serialNumber)

	// Сначала пробуем перезагрузить устройство
	cmd := exec.Command("cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to reboot device with cfgutil: %v\n", err)
	} else {
		fmt.Printf("Device rebooted, waiting before DFU instructions...\n")
		time.Sleep(5 * time.Second)
	}

	// Возвращаем инструкции для ручного входа в DFU
	return m.enterDFUManually(serialNumber)
}

func (m *Manager) enterDFUManually(serialNumber string) error {
	// Определяем тип Mac по серийному номеру или модели
	deviceInfo := m.getDeviceInfo(serialNumber)

	if strings.Contains(deviceInfo, "Apple Silicon") || strings.Contains(deviceInfo, "M1") || strings.Contains(deviceInfo, "M2") || strings.Contains(deviceInfo, "M3") {
		return fmt.Errorf("device %s requires manual DFU mode entry.\n\n"+
			"For Apple Silicon Macs:\n"+
			"1. Completely shut down the Mac\n"+
			"2. Connect the Mac to this computer via USB-C\n"+
			"3. Press and hold the power button\n"+
			"4. Keep holding until you see 'Loading startup options...'\n"+
			"5. Release the power button\n"+
			"6. The Mac should appear in DFU mode\n\n"+
			"Alternative method:\n"+
			"1. Shut down the Mac\n"+
			"2. Press and hold: Right Shift + Left Option + Left Control + Power for 10 seconds\n"+
			"3. Release all keys\n"+
			"4. Press the power button to start in DFU mode", serialNumber)
	} else {
		return fmt.Errorf("device %s requires manual DFU mode entry.\n\n"+
			"For Intel Macs:\n"+
			"1. Shut down the Mac completely\n"+
			"2. Connect the Mac to this computer via USB-C or Thunderbolt\n"+
			"3. Press and hold the power button for 10 seconds to ensure it's off\n"+
			"4. Press and hold the power button for 3 seconds\n"+
			"5. While still holding the power button, press and hold the volume down button\n"+
			"6. Hold both buttons for 10 seconds\n"+
			"7. Release the power button but continue holding volume down for 5 more seconds\n"+
			"8. The Mac should now be in DFU mode", serialNumber)
	}
}

func (m *Manager) getDeviceInfo(serialNumber string) string {
	// Пробуем получить информацию об устройстве через cfgutil
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return "Unknown"
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, serialNumber) {
			return line
		}
	}

	return "Unknown"
}

func (m *Manager) waitForDFUMode(serialNumber string) error {
	maxAttempts := 60 // Увеличиваем время ожидания до 2 минут
	fmt.Printf("Waiting for device %s to enter DFU mode...\n", serialNumber)

	for i := 0; i < maxAttempts; i++ {
		if m.isInDFUMode(serialNumber) {
			fmt.Printf("✅ Device %s successfully entered DFU mode\n", serialNumber)
			return nil
		}

		if i%10 == 0 { // Выводим сообщение каждые 20 секунд
			fmt.Printf("⏳ Attempt %d/%d: Waiting for device to enter DFU mode...\n", i+1, maxAttempts)
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("❌ device %s did not enter DFU mode within timeout (2 minutes)", serialNumber)
}

func (m *Manager) isInDFUMode(serialNumber string) bool {
	// Проверяем через cfgutil
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
			isDFU := strings.Contains(state, "dfu") || strings.Contains(state, "recovery")
			if isDFU {
				fmt.Printf("🔍 Device %s found in DFU/Recovery mode: %s\n", serialNumber, strings.TrimSpace(line))
			}
			return isDFU
		}
	}

	// Дополнительно проверяем через system_profiler для DFU устройств
	// Используем первый вариант если нужна точная проверка по серийному номеру:
	return m.checkDFUInSystemProfiler(serialNumber)

	// Или второй вариант если достаточно проверить наличие любых DFU устройств:
	// return m.checkDFUInSystemProfiler()
}

func (m *Manager) checkDFUInSystemProfiler() bool {
	cmd := exec.Command("system_profiler", "SPUSBDataType")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	outputStr := strings.ToLower(string(output))

	// Ищем DFU устройства
	if strings.Contains(outputStr, "dfu") || strings.Contains(outputStr, "recovery") {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(strings.ToLower(line), "dfu") || strings.Contains(strings.ToLower(line), "recovery") {
				fmt.Printf("🔍 Found DFU device in system_profiler: %s\n", strings.TrimSpace(line))
				return true
			}
		}
	}

	return false
}

func (m *Manager) IsInDFUMode(serialNumber string) bool {
	return m.isInDFUMode(serialNumber)
}
