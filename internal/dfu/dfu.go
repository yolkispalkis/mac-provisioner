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
	fmt.Printf("Using macvdmtool to enter DFU mode for device %s\n", serialNumber)

	cmd := exec.Command("macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute macvdmtool dfu: %w", err)
	}

	fmt.Printf("macvdmtool dfu command executed successfully\n")
	time.Sleep(15 * time.Second)
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) enterDFUWithCfgutil(serialNumber string) error {
	fmt.Printf("macvdmtool not available, using cfgutil for device %s\n", serialNumber)

	cmd := exec.Command("cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to reboot device with cfgutil: %v\n", err)
	} else {
		fmt.Printf("Device rebooted, waiting before DFU instructions...\n")
		time.Sleep(5 * time.Second)
	}

	return m.enterDFUManually(serialNumber)
}

func (m *Manager) enterDFUManually(serialNumber string) error {
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
	maxAttempts := 60
	fmt.Printf("Waiting for device %s to enter DFU mode...\n", serialNumber)

	for i := 0; i < maxAttempts; i++ {
		if m.isInDFUMode(serialNumber) {
			fmt.Printf("✅ Device %s successfully entered DFU mode\n", serialNumber)
			return nil
		}

		if i%10 == 0 {
			fmt.Printf("⏳ Attempt %d/%d: Waiting for device to enter DFU mode...\n", i+1, maxAttempts)
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("❌ device %s did not enter DFU mode within timeout (2 minutes)", serialNumber)
}

func (m *Manager) isInDFUMode(serialNumber string) bool {
	// Проверяем, есть ли DFU устройства
	dfuDevices := m.GetDFUDevices()
	return len(dfuDevices) > 0
}

func (m *Manager) IsInDFUMode(serialNumber string) bool {
	return m.isInDFUMode(serialNumber)
}

// Получаем список DFU устройств с их ECID
func (m *Manager) GetDFUDevices() []DFUDevice {
	var dfuDevices []DFUDevice

	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return dfuDevices
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Name") {
			continue
		}

		// Парсим строку типа: "Type: MacBookAir10,1	ECID: 0xC599E36BB001E	UDID: N/A Location: 0x100000 Name: N/A"
		if strings.Contains(line, "Type:") && strings.Contains(line, "ECID:") {
			device := m.parseDFULine(line)
			if device.ECID != "" {
				dfuDevices = append(dfuDevices, device)
				fmt.Printf("🔍 Found DFU device: Type=%s, ECID=%s\n", device.Type, device.ECID)
			}
		}
	}

	return dfuDevices
}

type DFUDevice struct {
	Type string
	ECID string
	UDID string
}

func (m *Manager) parseDFULine(line string) DFUDevice {
	device := DFUDevice{}

	// Разбираем строку по табуляции или пробелам
	parts := strings.Fields(line)

	for i, part := range parts {
		if part == "Type:" && i+1 < len(parts) {
			device.Type = parts[i+1]
		} else if part == "ECID:" && i+1 < len(parts) {
			device.ECID = parts[i+1]
		} else if part == "UDID:" && i+1 < len(parts) {
			device.UDID = parts[i+1]
		}
	}

	return device
}

// Получаем первый доступный ECID для восстановления
func (m *Manager) GetFirstDFUECID() string {
	devices := m.GetDFUDevices()
	if len(devices) > 0 {
		return devices[0].ECID
	}
	return ""
}
