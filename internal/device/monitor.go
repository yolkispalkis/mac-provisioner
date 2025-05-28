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
	firstScan    bool
}

func NewMonitor(cfg config.MonitoringConfig) *Monitor {
	return &Monitor{
		config:    cfg,
		eventChan: make(chan Event, cfg.EventBufferSize),
		devices:   make(map[string]*Device),
		firstScan: true,
	}
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}

	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB устройств...")

	// Проверяем доступность cfgutil
	if err := m.checkCfgutilAvailable(); err != nil {
		log.Printf("⚠️ Предупреждение: %v", err)
	}

	// Начальное сканирование
	if err := m.initialScan(); err != nil {
		log.Printf("⚠️ Предупреждение: ошибка начального сканирования: %v", err)
	}

	// Запускаем мониторинг изменений
	go m.monitorLoop()

	// Запускаем очистку
	go m.cleanupLoop()

	log.Println("✅ Мониторинг USB устройств запущен")
	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}

	log.Println("🛑 Остановка мониторинга USB...")
	m.running = false

	if m.cancel != nil {
		m.cancel()
	}

	close(m.eventChan)
}

func (m *Monitor) Events() <-chan Event {
	return m.eventChan
}

func (m *Monitor) checkCfgutilAvailable() error {
	cmd := exec.Command("cfgutil", "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cfgutil недоступен. Убедитесь, что Apple Configurator 2 установлен")
	}
	log.Println("✅ cfgutil доступен")
	return nil
}

func (m *Monitor) monitorLoop() {
	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	log.Printf("🔄 Запуск цикла мониторинга с интервалом %v", m.config.CheckInterval)

	for {
		select {
		case <-m.ctx.Done():
			log.Println("🛑 Цикл мониторинга остановлен")
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

	// Если это первое сканирование после запуска, генерируем события подключения
	// для всех найденных устройств
	if m.firstScan {
		log.Println("🔍 Первое сканирование - генерируем события для всех найденных устройств")
		for serial, dev := range currentMap {
			m.devices[serial] = dev
			log.Printf("🆕 Устройство найдено при запуске: %s (%s) - %s", serial, dev.Model, dev.State)
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	// Проверяем новые устройства
	for serial, dev := range currentMap {
		if existing, exists := m.devices[serial]; !exists {
			log.Printf("🆕 Новое устройство обнаружено: %s (%s)", serial, dev.Model)
			m.devices[serial] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if existing.State != dev.State || existing.IsDFU != dev.IsDFU {
			log.Printf("🔄 Изменение состояния устройства: %s (%s) %s -> %s", serial, dev.Model, existing.State, dev.State)
			m.devices[serial] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	// Проверяем отключенные устройства
	for serial, dev := range m.devices {
		if _, exists := currentMap[serial]; !exists {
			log.Printf("🔌 Устройство отключено: %s (%s)", serial, dev.Model)
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

	// Получаем DFU устройства отдельно
	dfuDevices := m.getDFUDevices()
	devices = append(devices, dfuDevices...)

	return m.removeDuplicates(devices)
}

func (m *Monitor) getCfgutilDevices() []*Device {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("❌ Ошибка выполнения cfgutil list: %v", err)
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
		if line == "" {
			continue
		}

		// Пропускаем DFU устройства (они обрабатываются отдельно)
		if strings.HasPrefix(line, "Type:") && strings.Contains(line, "ECID:") {
			continue
		}

		// Пропускаем заголовки
		if strings.HasPrefix(line, "ECID") || strings.HasPrefix(line, "Name") {
			continue
		}

		device := m.parseDeviceLine(line)
		if device != nil && !device.IsDFU {
			devices = append(devices, device)
			log.Printf("✅ Обычное устройство из cfgutil: %s (%s) - %s", device.SerialNumber, device.Model, device.State)
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
			log.Printf("✅ DFU устройство: %s (ECID: %s)", device.Model, device.ECID)
		}
	}

	return devices
}

func (m *Monitor) parseDeviceLine(line string) *Device {
	device := &Device{}

	// Парсим формат с табуляцией (новый формат cfgutil)
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

	// Парсим старый формат cfgutil
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
		log.Printf("📤 Отправлено событие: %s для устройства %s", event.Type, event.Device.SerialNumber)
	default:
		log.Println("⚠️ Буфер событий переполнен, событие пропущено")
	}
}

func (m *Monitor) initialScan() error {
	log.Println("🔍 Выполнение начального сканирования...")
	devices := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// При начальном сканировании просто сохраняем устройства,
	// события будут сгенерированы при первой проверке
	for _, dev := range devices {
		if dev.IsValidSerial() {
			log.Printf("📱 Найдено при начальном сканировании: %s (%s) - %s", dev.SerialNumber, dev.Model, dev.State)
		}
	}

	log.Printf("✅ Начальное сканирование завершено: найдено %d устройств", len(devices))
	return nil
}

func (m *Monitor) cleanup() {
	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	log.Printf("🧹 Очистка: отслеживается %d устройств", len(m.devices))
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
