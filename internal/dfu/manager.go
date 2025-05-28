package dfu

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"mac-provisioner/internal/device"
)

type Manager struct{}

func New() *Manager { return &Manager{} }

/* ============================================================
   ВХОД В DFU
   ============================================================ */

func (m *Manager) EnterDFUMode(serialNumber string) error {
	// 1. Пытаемся через macvdmtool
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(serialNumber)
	}

	// 2. Fallback → cfgutil reboot
	return m.enterDFUWithCfgutil(serialNumber)
}

/* -------------------- macvdmtool -------------------- */

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(serialNumber string) error {
	fmt.Printf("Использование macvdmtool для входа в DFU режим устройства %s\n", serialNumber)

	// Сначала пробуем без sudo
	cmd := exec.Command("macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		// Попытка с sudo (понадобится NOPASSWD или открытый парольный кеш)
		fmt.Printf("macvdmtool без sudo не удался: %v, пробуем sudo…\n", err)
		cmd = exec.Command("sudo", "-n", "macvdmtool", "dfu")
		if err2 := cmd.Run(); err2 != nil {
			return fmt.Errorf("ошибка выполнения macvdmtool dfu: %w", err2)
		}
	}

	fmt.Println("Команда macvdmtool dfu выполнена успешно, ждём 15 с…")
	time.Sleep(15 * time.Second)

	return m.waitForDFUMode(serialNumber)
}

/* -------------------- cfgutil fallback -------------------- */

func (m *Manager) enterDFUWithCfgutil(serialNumber string) error {
	fmt.Printf("macvdmtool недоступен — используем cfgutil reboot для %s\n", serialNumber)

	cmd := exec.Command("cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Ошибка reboot через cfgutil: %v\n", err)
	} else {
		fmt.Println("Устройство перезагружено. Через 5 с покажем инструкции DFU…")
		time.Sleep(5 * time.Second)
	}

	// Падение в ручной режим
	return m.enterDFUManually(serialNumber)
}

/* ============================================================
   MANUAL DFU (инструкции пользователю)
   ============================================================ */

func (m *Manager) enterDFUManually(serialNumber string) error {
	info := m.getDeviceInfo(serialNumber)

	if m.isAppleSilicon(info) {
		return fmt.Errorf(`устройство %s требует ручного входа в DFU режим.

Для Mac на Apple Silicon:
 1. Полностью выключите Mac
 2. Подключите его к этому компьютеру кабелем USB-C
 3. Нажмите и удерживайте кнопку питания — удерживайте пока не появится «Загрузка вариантов запуска…»
 4. Отпустите кнопку питания — Mac должен появиться в DFU режиме

Альтернативно:
 1. Выключите Mac
 2. Удерживайте: Правый Shift + Левый Option + Левый Control + Питание (10 с)
 3. Отпустите клавиши, затем снова нажмите Питание`, serialNumber)
	}

	return fmt.Errorf(`устройство %s требует ручного входа в DFU режим.

Для Intel-Mac:
 1. Полностью выключите Mac
 2. Подключите через USB-C/Thunderbolt
 3. Нажмите и удерживайте кнопку питания 10 с
 4. Затем нажмите и удерживайте кнопку питания 3 с
 5. Не отпуская питание, нажмите и держите кнопку уменьшения громкости (10 с)
 6. Отпустите питание, громкость держите ещё 5 с
 7. Mac должен войти в DFU`, serialNumber)
}

/* -------------------- вспомогательные -------------------- */

func (m *Manager) getDeviceInfo(serialNumber string) string {
	out, err := exec.Command("cfgutil", "list").Output()
	if err != nil {
		return "Unknown"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, serialNumber) {
			return line
		}
	}
	return "Unknown"
}

func (m *Manager) isAppleSilicon(info string) bool {
	inf := strings.ToLower(info)
	return strings.Contains(inf, "apple silicon") ||
		strings.Contains(inf, "m1") ||
		strings.Contains(inf, "m2") ||
		strings.Contains(inf, "m3") ||
		strings.Contains(inf, "m4")
}

/* ============================================================
   WAIT HELPERS
   ============================================================ */

func (m *Manager) waitForDFUMode(serialNumber string) error {
	const maxAttempts = 60
	fmt.Printf("Ожидание появления устройства %s в DFU (до 2 мин)…\n", serialNumber)

	for i := 0; i < maxAttempts; i++ {
		if m.isInDFUMode() {
			fmt.Printf("✅ Устройство %s в DFU режиме\n", serialNumber)
			return nil
		}
		if i%10 == 0 {
			fmt.Printf("⏳ %d / %d попыток…\n", i+1, maxAttempts)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("устройство %s не вошло в DFU за 2 мин", serialNumber)
}

func (m *Manager) isInDFUMode() bool {
	return len(m.GetDFUDevices()) > 0
}

/* ============================================================
   ЧТЕНИЕ DFU-УСТРОЙСТВ
   ============================================================ */

func (m *Manager) GetDFUDevices() []*device.Device {
	var list []*device.Device

	out, err := exec.Command("cfgutil", "list").Output()
	if err != nil {
		return list
	}

	for _, line := range strings.Split(string(out), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "Name") {
			continue
		}
		if strings.Contains(l, "Type:") && strings.Contains(l, "ECID:") {
			if d := m.parseDFULine(l); d != nil && d.ECID != "" {
				list = append(list, d)
			}
		}
	}
	return list
}

func (m *Manager) parseDFULine(line string) *device.Device {
	d := &device.Device{IsDFU: true, State: "DFU"}

	fields := strings.Fields(line)
	for i, f := range fields {
		switch f {
		case "Type:":
			if i+1 < len(fields) {
				d.Model = fields[i+1]
			}
		case "ECID:":
			if i+1 < len(fields) {
				d.ECID = fields[i+1]
				d.SerialNumber = "DFU-" + fields[i+1]
			}
		}
	}
	return d
}

func (m *Manager) GetFirstDFUECID() string {
	if devs := m.GetDFUDevices(); len(devs) > 0 {
		return devs[0].ECID
	}
	return ""
}
