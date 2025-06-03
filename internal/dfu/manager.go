package dfu

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	lastTrigger  time.Time
	triggerMutex sync.Mutex
	debugMode    bool
}

func New() *Manager {
	return &Manager{
		debugMode: os.Getenv("MAC_PROV_DEBUG") == "1",
	}
}

// AutoTriggerDFU автоматически запускает macvdmtool dfu
func (m *Manager) AutoTriggerDFU(ctx context.Context) {
	m.triggerMutex.Lock()
	defer m.triggerMutex.Unlock()

	// Защита от слишком частых вызовов
	if time.Since(m.lastTrigger) < 2*time.Second {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Автоматический DFU пропущен (слишком частые вызовы)")
		}
		return
	}
	m.lastTrigger = time.Now()

	if !m.hasMacvdmtool() {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] macvdmtool недоступен для автоматического DFU")
		}
		return
	}

	if m.debugMode {
		log.Printf("🔍 [DEBUG] Выполняем: macvdmtool dfu")
	}

	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Автоматический macvdmtool завершился с ошибкой: %v", err)
		}
	} else {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Автоматический macvdmtool выполнен успешно")
		}
	}
}

func (m *Manager) EnterDFUMode(ctx context.Context, usbLocation string) error {
	if !m.isDFUPort(usbLocation) {
		humanPort := m.humanPort(usbLocation)
		return fmt.Errorf("устройство подключено к %s, автоматический переход в DFU невозможен", humanPort)
	}

	if !m.hasMacvdmtool() {
		return errors.New("macvdmtool недоступен")
	}

	humanPort := m.humanPort(usbLocation)
	log.Printf("🔄 Ручной запуск перехода в DFU режим (%s)...", humanPort)

	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Ошибка macvdmtool: %v", err)
		}
		return errors.New("не удалось перевести устройство в DFU режим")
	}

	return m.waitForDFUMode(ctx, 2*time.Minute)
}

func (m *Manager) isDFUPort(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}

	parts := strings.Split(usbLocation, "/")
	if len(parts) == 0 {
		return false
	}

	baseLocation := strings.ToLower(strings.TrimPrefix(parts[0], "0x"))

	for len(baseLocation) < 8 {
		baseLocation = "0" + baseLocation
	}

	return strings.HasPrefix(baseLocation, "00100000")
}

func (m *Manager) CheckDFUPortCompatibility(usbLocation string) (bool, string) {
	if usbLocation == "" {
		return false, "USB порт не определен"
	}

	humanPort := m.humanPort(usbLocation)
	isDFU := m.isDFUPort(usbLocation)

	if isDFU {
		return true, fmt.Sprintf("Устройство подключено к %s (поддерживает автоматический DFU)", humanPort)
	} else {
		return false, fmt.Sprintf("Устройство подключено к %s (не поддерживает автоматический DFU)", humanPort)
	}
}

func (m *Manager) humanPort(loc string) string {
	if loc == "" {
		return "неизвестный порт"
	}

	base := strings.Split(loc, "/")[0]
	base = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(base), "0x"))
	if base == "" {
		return "неизвестный порт"
	}

	switch {
	case len(base) < 8:
		base = strings.Repeat("0", 8-len(base)) + base
	case len(base) > 8:
		base = base[len(base)-8:]
	}

	var ports []int
	for i := 0; i < len(base); i++ {
		v, err := strconv.ParseInt(base[i:i+1], 16, 0)
		if err != nil {
			return "неизвестный порт"
		}
		if v != 0 {
			ports = append(ports, int(v))
		}
	}

	if len(ports) == 0 {
		return "неизвестный порт"
	}

	root := ports[0]

	switch root {
	case 1:
		if len(ports) == 1 {
			return "встроенный USB-C порт 1"
		}
		hubs := make([]string, len(ports)-1)
		for i, p := range ports[1:] {
			hubs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("встроенный порт 1, хаб %s", strings.Join(hubs, "-"))

	case 2:
		if len(ports) == 1 {
			return "внешний хаб (порт 2)"
		}
		hubs := make([]string, len(ports)-1)
		for i, p := range ports[1:] {
			hubs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("внешний хаб (порт 2), подпорт %s", strings.Join(hubs, "-"))

	case 3:
		if len(ports) == 1 {
			return "внешний хаб (порт 3)"
		}
		hubs := make([]string, len(ports)-1)
		for i, p := range ports[1:] {
			hubs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("внешний хаб (порт 3), подпорт %s", strings.Join(hubs, "-"))

	case 4:
		if len(ports) == 1 {
			return "внешний хаб (порт 4)"
		}
		hubs := make([]string, len(ports)-1)
		for i, p := range ports[1:] {
			hubs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("внешний хаб (порт 4), подпорт %s", strings.Join(hubs, "-"))

	default:
		if len(ports) == 1 {
			return fmt.Sprintf("порт %d", root)
		}
		hubs := make([]string, len(ports)-1)
		for i, p := range ports[1:] {
			hubs[i] = strconv.Itoa(p)
		}
		return fmt.Sprintf("хаб %s, порт %d", strings.Join(hubs, "-"), root)
	}
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) waitForDFUMode(ctx context.Context, timeout time.Duration) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeoutTimer.C:
			return errors.New("таймаут ожидания DFU режима")
		case <-ticker.C:
			if m.isDFUModeActive(ctx) {
				log.Printf("✅ Устройство перешло в DFU режим")
				return nil
			}
		}
	}
}

func (m *Manager) isDFUModeActive(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	outputStr := strings.ToLower(string(output))
	return strings.Contains(outputStr, "dfu mode")
}
