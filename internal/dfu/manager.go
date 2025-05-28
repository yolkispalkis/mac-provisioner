package dfu

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"mac-provisioner/internal/device"
)

type Manager struct{}

func New() *Manager {
	return &Manager{}
}

func (m *Manager) EnterDFUMode(serialNumber string) error {
	// Сначала пробуем macvdmtool
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(serialNumber)
	}

	// Если macvdmtool недоступен, используем cfgutil для перезагрузки
	return m.enterDFUWithCfgutil(serialNumber)
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(serialNumber string) error {
	fmt.Printf("Использование macvdmtool для входа в DFU режим устройства %s\n", serialNumber)

	cmd := exec.Command("macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ошибка выполнения macvdmtool dfu: %w", err)
	}

	fmt.Printf("Команда macvdmtool dfu выполнена успешно\n")
	time.Sleep(15 * time.Second)
	return m.waitForDFUMode(serialNumber)
}

func (m *Manager) enterDFUWithCfgutil(serialNumber string) error {
	fmt.Printf("macvdmtool недоступен, используется cfgutil для устройства %s\n", serialNumber)

	cmd := exec.Command("cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Ошибка перезагрузки устройства через cfgutil: %v\n", err)
	} else {
		fmt.Printf("Устройство перезагружено, ожидание перед инструкциями DFU...\n")
		time.Sleep(5 * time.Second)
	}

	return m.enterDFUManually(serialNumber)
}

func (m *Manager) enterDFUManually(serialNumber string) error {
	deviceInfo := m.getDeviceInfo(serialNumber)

	if m.isAppleSilicon(deviceInfo) {
		return fmt.Errorf("устройство %s требует ручного входа в DFU режим.\n\n"+
			"Для Mac на Apple Silicon:\n"+
			"1. Полностью выключите Mac\n"+
			"2. Подключите Mac к этому компьютеру через USB-C\n"+
			"3. Нажмите и удерживайте кнопку питания\n"+
			"4. Удерживайте до появления 'Загрузка вариантов запуска...'\n"+
			"5. Отпустите кнопку питания\n"+
			"6. Mac должен появиться в DFU режиме\n\n"+
			"Альтернативный метод:\n"+
			"1. Выключите Mac\n"+
			"2. Нажмите и удерживайте: Правый Shift + Левый Option + Левый Control + Питание в течение 10 секунд\n"+
			"3. Отпустите все клавиши\n"+
			"4. Нажмите кнопку питания для запуска в DFU режиме", serialNumber)
	} else {
		return fmt.Errorf("устройство %s требует ручного входа в DFU режим.\n\n"+
			"Для Intel Mac:\n"+
			"1. Полностью выключите Mac\n"+
			"2. Подключите Mac к этому компьютеру через USB-C или Thunderbolt\n"+
			"3. Нажмите и удерживайте кнопку питания 10 секунд для полного выключения\n"+
			"4. Нажмите и удерживайте кнопку питания 3 секунды\n"+
			"5. Продолжая удерживать кнопку питания, нажмите и удерживайте кнопку уменьшения громкости\n"+
			"6. Удерживайте обе кнопки 10 секунд\n"+
			"7. Отпустите кнопку питания, но продолжайте удерживать кнопку громкости еще 5 секунд\n"+
			"8. Mac должен войти в DFU режим", serialNumber)
	}
}

func (m *Manager) isAppleSilicon(deviceInfo string) bool {
	info := strings.ToLower(deviceInfo)
	return strings.Contains(info, "apple silicon") ||
		strings.Contains(info, "m1") ||
		strings.Contains(info, "m2") ||
		strings.Contains(info, "m3") ||
		strings.Contains(info, "m4")
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
	fmt.Printf("Ожидание входа устройства %s в DFU режим...\n", serialNumber)

	for i := 0; i < maxAttempts; i++ {
		if m.isInDFUMode() {
			fmt.Printf("✅ Устройство %s успешно вошло в DFU режим\n", serialNumber)
			return nil
		}

		if i%10 == 0 {
			fmt.Printf("⏳ Попытка %d/%d: Ожидание входа устройства в DFU режим...\n", i+1, maxAttempts)
		}

		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("❌ устройство %s не вошло в DFU режим в течение таймаута (2 минуты)", serialNumber)
}

func (m *Manager) isInDFUMode() bool {
	dfuDevices := m.GetDFUDevices()
	return len(dfuDevices) > 0
}

func (m *Manager) GetDFUDevices() []*device.Device {
	var dfuDevices []*device.Device

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

		if strings.Contains(line, "Type:") && strings.Contains(line, "ECID:") {
			device := m.parseDFULine(line)
			if device != nil && device.ECID != "" {
				dfuDevices = append(dfuDevices, device)
				fmt.Printf("🔍 Найдено DFU устройство: Type=%s, ECID=%s\n", device.Model, device.ECID)
			}
		}
	}

	return dfuDevices
}

func (m *Manager) parseDFULine(line string) *device.Device {
	dev := &device.Device{
		IsDFU: true,
		State: "DFU",
	}

	parts := strings.Fields(line)

	for i, part := range parts {
		if part == "Type:" && i+1 < len(parts) {
			dev.Model = parts[i+1]
		} else if part == "ECID:" && i+1 < len(parts) {
			dev.ECID = parts[i+1]
			dev.SerialNumber = "DFU-" + parts[i+1]
		}
	}

	return dev
}

func (m *Manager) GetFirstDFUECID() string {
	devices := m.GetDFUDevices()
	if len(devices) > 0 {
		return devices[0].ECID
	}
	return ""
}
