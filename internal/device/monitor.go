package device

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
)

const (
	EventConnected    = "connected"
	EventDisconnected = "disconnected"
	EventStateChanged = "state_changed"
)

type Event struct {
	Type   string  `json:"type"`
	Device *Device `json:"device"`
}

type Monitor struct {
	config       config.MonitoringConfig
	eventChan    chan Event
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	running      bool
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewMonitor(cfg config.MonitoringConfig) *Monitor {
	return &Monitor{
		config:    cfg,
		eventChan: make(chan Event, cfg.EventBufferSize),
		devices:   make(map[string]*Device),
	}
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}

	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("Запуск мониторинга USB устройств...")

	// Начальное сканирование
	if err := m.initialScan(); err != nil {
		log.Printf("Предупреждение: ошибка начального сканирования: %v", err)
	}

	// Запускаем мониторинг изменений
	go m.monitorLoop()

	// Запускаем очистку
	go m.cleanupLoop()

	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}

	log.Println("Остановка мониторинга USB...")
	m.running = false

	if m.cancel != nil {
		m.cancel()
	}

	close(m.eventChan)
}

func (m *Monitor) Events() <-chan Event {
	return m.eventChan
}

func (m *Monitor) monitorLoop() {
	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkDevices()
		}
	}
}

func (m *Monitor) cleanupLoop() {
	ticker := time.NewTicker(m.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.cleanup()
		}
	}
}

func (m *Monitor) checkDevices() {
	currentDevices := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// Создаем карту текущих устройств
	currentMap := make(map[string]*Device)
	for _, dev := range currentDevices {
		if dev.IsValidSerial() {
			currentMap[dev.SerialNumber] = dev
		}
	}

	// Проверяем новые устройства
	for serial, dev := range currentMap {
		if existing, exists := m.devices[serial]; !exists {
			m.devices[serial] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if existing.State != dev.State || existing.IsDFU != dev.IsDFU {
			m.devices[serial] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	// Проверяем отключенные устройства
	for serial, dev := range m.devices {
		if _, exists := currentMap[serial]; !exists {
			delete(m.devices, serial)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
		}
	}
}

func (m *Monitor) getCurrentDevices() []*Device {
	var devices []*Device

	// Получаем устройства из cfgutil
	cfgutilDevices := m.getCfgutilDevices()
	devices = append(devices, cfgutilDevices...)

	// Получаем DFU устройства
	dfuDevices := m.getDFUDevices()
	devices = append(devices, dfuDevices...)

	return m.removeDuplicates(devices)
}

func (m *Monitor) getCfgutilDevices() []*Device {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	return m.parseCfgutilOutput(string(output))
}

func (m *Monitor) getDFUDevices() []*Device {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil
	}

	return m.parseDFUOutput(string(output))
}

func (m *Monitor) parseCfgutilOutput(output string) []*Device {
	var devices []*Device
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ECID") || strings.HasPrefix(line, "Name") {
			continue
		}

		device := m.parseDeviceLine(line)
		if device != nil && !device.IsDFU {
			devices = append(devices, device)
		}
	}

	return devices
}

func (m *Monitor) parseDFUOutput(output string) []*Device {
	var devices []*Device
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "Type:") || !strings.Contains(line, "ECID:") {
			continue
		}

		device := m.parseDFULine(line)
		if device != nil {
			devices = append(devices, device)
		}
	}

	return devices
}

func (m *Monitor) parseDeviceLine(line string) *Device {
	device := &Device{}

	// Парсим формат с табуляцией
	if strings.Contains(line, "\t") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 {
			device.SerialNumber = strings.TrimSpace(parts[0])
			device.Model = strings.TrimSpace(parts[1])
			device.State = strings.TrimSpace(parts[2])

			state := strings.ToLower(device.State)
			device.IsDFU = strings.Contains(state, "dfu") || strings.Contains(state, "recovery")

			return device
		}
	}

	// Парсим обычный формат
	parts := strings.Fields(line)
	if len(parts) >= 1 {
		device.SerialNumber = parts[0]

		// Извлекаем модель из скобок
		if start := strings.Index(line, "("); start != -1 {
			if end := strings.Index(line[start:], ")"); end != -1 {
				device.Model = line[start+1 : start+end]
			}
		}

		// Извлекаем состояние после последнего дефиса
		if dashIndex := strings.LastIndex(line, " - "); dashIndex != -1 {
			device.State = strings.TrimSpace(line[dashIndex+3:])
		} else {
			device.State = "Unknown"
		}

		state := strings.ToLower(device.State)
		device.IsDFU = strings.Contains(state, "dfu") || strings.Contains(state, "recovery")

		return device
	}

	return nil
}

func (m *Monitor) parseDFULine(line string) *Device {
	device := &Device{
		IsDFU: true,
		State: "DFU",
	}

	// Разбираем строку по полям
	parts := strings.Fields(line)

	for i, part := range parts {
		if part == "Type:" && i+1 < len(parts) {
			device.Model = parts[i+1]
		} else if part == "ECID:" && i+1 < len(parts) {
			device.ECID = parts[i+1]
			// Для DFU устройств используем ECID как серийный номер
			device.SerialNumber = "DFU-" + parts[i+1]
		}
	}

	return device
}

func (m *Monitor) removeDuplicates(devices []*Device) []*Device {
	seen := make(map[string]bool)
	var result []*Device

	for _, device := range devices {
		if device.SerialNumber != "" && !seen[device.SerialNumber] {
			seen[device.SerialNumber] = true
			result = append(result, device)
		}
	}

	return result
}

func (m *Monitor) sendEvent(event Event) {
	select {
	case m.eventChan <- event:
	default:
		log.Println("Буфер событий переполнен, событие пропущено")
	}
}

func (m *Monitor) initialScan() error {
	devices := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	for _, dev := range devices {
		if dev.IsValidSerial() {
			m.devices[dev.SerialNumber] = dev
		}
	}

	log.Printf("Начальное сканирование обнаружило %d устройств", len(devices))
	return nil
}

func (m *Monitor) cleanup() {
	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// Здесь можно добавить логику очистки устаревших записей
	log.Printf("Очистка: отслеживается %d устройств", len(m.devices))
}

func (m *Monitor) GetConnectedDevices() []*Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	var devices []*Device
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}

	return devices
}
