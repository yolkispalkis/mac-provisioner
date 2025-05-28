package device

import (
	"bytes"
	"context"
	"encoding/json"
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

const (
	appleVendorID     = "0x05ac"
	dfuModePIDAS      = "0x1281"
	recoveryModePIDAS = "0x1280"
	dfuModePIDIntelT2 = "0x1227"
)

type SPUSBItem struct {
	Name       string      `json:"_name"`
	ProductID  string      `json:"product_id,omitempty"`
	VendorID   string      `json:"vendor_id,omitempty"`
	SerialNum  string      `json:"serial_num,omitempty"`
	LocationID string      `json:"location_id,omitempty"`
	SubItems   []SPUSBItem `json:"_items,omitempty"`
}

type SPUSBDataType struct {
	Items []SPUSBItem `json:"SPUSBDataType"`
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}
	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB устройств (через system_profiler)...")

	if err := m.checkCfgutilStillNeeded(); err != nil {
		log.Printf("⚠️ %v (cfgutil все еще нужен для операций восстановления)", err)
		// Можно решить, что это фатальная ошибка, если cfgutil критичен
		// return fmt.Errorf("cfgutil недоступен: %w", err)
	}

	if err := m.initialScan(); err != nil {
		log.Printf("⚠️ Начальное сканирование (system_profiler): %v", err)
	}

	go m.monitorLoop()
	go m.cleanupLoop() // Renamed from cleanupStaleDevices for clarity

	log.Println("✅ Мониторинг USB устройств (system_profiler) запущен")
	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}
	log.Println("🛑 Остановка мониторинга USB (system_profiler)...")
	m.running = false
	if m.cancel != nil {
		m.cancel()
	}
	// Закрытие eventChan лучше делать здесь, если уверены, что все писатели остановлены.
	// Но так как main.go читает из него, и он завершается по ctx.Done(),
	// явное закрытие может быть излишним или привести к панике при записи в закрытый канал,
	// если Stop() вызывается до завершения всех писателей.
	// Если Stop() гарантированно вызывается после завершения ctx, то можно закрыть.
	// Оставим без close(m.eventChan) пока, полагаясь на завершение читателей по ctx.Done().
}

func (m *Monitor) Events() <-chan Event { return m.eventChan }

func (m *Monitor) checkCfgutilStillNeeded() error {
	if _, err := exec.LookPath("cfgutil"); err == nil {
		log.Println("✅ cfgutil доступен (найден в $PATH) и будет использован для операций восстановления.")
		return nil
	}
	return fmt.Errorf("cfgutil недоступен в $PATH. Установите Apple Configurator, он необходим для операций восстановления")
}

func (m *Monitor) monitorLoop() {
	t := time.NewTicker(m.config.CheckInterval)
	defer t.Stop()
	log.Printf("🔄 Цикл мониторинга (system_profiler) %v", m.config.CheckInterval)

	for {
		select {
		case <-m.ctx.Done():
			log.Println("🛑 Цикл мониторинга (system_profiler) остановлен")
			return
		case <-t.C:
			m.checkDevices()
		}
	}
}

func (m *Monitor) cleanupLoop() {
	t := time.NewTicker(m.config.CleanupInterval)
	defer t.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.devicesMutex.RLock()
			log.Printf("🧹 Периодическая проверка: отслеживается %d устройств (system_profiler)", len(m.devices))
			m.devicesMutex.RUnlock()
		}
	}
}

func (m *Monitor) checkDevices() {
	currentSPDevices := m.fetchCurrentUSBDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	currentDeviceMap := make(map[string]*Device, len(currentSPDevices))
	for _, dev := range currentSPDevices {
		if dev.SerialNumber != "" {
			currentDeviceMap[dev.SerialNumber] = dev
		} else {
			log.Printf("Предупреждение: Обнаружено устройство без серийного номера/ECID: %s", dev.Model)
		}
	}

	if m.firstScan {
		log.Println("🔍 Первое сканирование (system_profiler) — генерируем события Connected")
		for sn, dev := range currentDeviceMap {
			m.devices[sn] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	for sn, currentDev := range currentDeviceMap {
		oldDev, exists := m.devices[sn]
		if !exists {
			m.devices[sn] = currentDev
			m.sendEvent(Event{Type: EventConnected, Device: currentDev})
		} else {
			if oldDev.State != currentDev.State || oldDev.IsDFU != currentDev.IsDFU || oldDev.USBLocation != currentDev.USBLocation {
				m.devices[sn] = currentDev
				m.sendEvent(Event{Type: EventStateChanged, Device: currentDev})
			}
		}
	}

	for sn, oldDev := range m.devices {
		if _, exists := currentDeviceMap[sn]; !exists {
			delete(m.devices, sn)
			m.sendEvent(Event{Type: EventDisconnected, Device: oldDev})
		}
	}
}

func (m *Monitor) fetchCurrentUSBDevices() []*Device {
	cmd := exec.CommandContext(m.ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if m.ctx.Err() == context.Canceled {
			return nil
		}
		log.Printf("❌ Ошибка выполнения system_profiler SPUSBDataType: %v, stderr: %s", err, stderr.String())
		return nil
	}

	var data SPUSBDataType
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		log.Printf("❌ Ошибка парсинга JSON из system_profiler: %v", err)
		return nil
	}

	var detectedDevices []*Device
	for _, usbControllerInfo := range data.Items {
		m.extractDevicesRecursively(&usbControllerInfo, &detectedDevices)
	}
	return detectedDevices
}

func (m *Monitor) extractDevicesRecursively(spItem *SPUSBItem, devices *[]*Device) {
	if strings.EqualFold(spItem.VendorID, appleVendorID) {
		pidLower := strings.ToLower(spItem.ProductID)
		isDFUMode := false
		deviceState := "Unknown"
		deviceModel := spItem.Name

		switch pidLower {
		case dfuModePIDAS:
			isDFUMode = true
			deviceState = "DFU"
			deviceModel = "Apple Silicon (DFU Mode)"
		case recoveryModePIDAS:
			isDFUMode = true
			deviceState = "Recovery"
			deviceModel = "Apple Silicon (Recovery Mode)"
		case dfuModePIDIntelT2:
			isDFUMode = true
			deviceState = "DFU"
			deviceModel = "Intel T2 (DFU Mode)"
		}

		if isDFUMode {
			dev := &Device{
				Model:       deviceModel,
				State:       deviceState,
				IsDFU:       true,
				USBLocation: spItem.LocationID,
			}
			if spItem.SerialNum != "" {
				dev.ECID = spItem.SerialNum
				dev.SerialNumber = "DFU-" + strings.TrimPrefix(strings.ToLower(dev.ECID), "0x")
			} else {
				log.Printf("⚠️ Обнаружено DFU-устройство (%s) без serial_num (ECID) в system_profiler: %s. Оно будет проигнорировано.", deviceModel, spItem.Name)
			}

			if dev.ECID != "" {
				*devices = append(*devices, dev)
			}
		}
	}

	if spItem.SubItems != nil {
		for i := range spItem.SubItems {
			m.extractDevicesRecursively(&spItem.SubItems[i], devices)
		}
	}
}

// removeDuplicates был удален, так как не использовался.

func (m *Monitor) sendEvent(e Event) {
	// Делаем неблокирующую отправку с таймаутом, чтобы не зависнуть, если канал переполнен
	// и контекст еще не отменен.
	sendTimeout := time.NewTimer(100 * time.Millisecond) // Короткий таймаут
	defer sendTimeout.Stop()

	select {
	case m.eventChan <- e:
		// log.Printf("📤 Событие отправлено: %s для %s", e.Type, e.Device.SerialNumber)
	case <-m.ctx.Done():
		log.Println("ℹ️ Канал событий не принимает (контекст завершен), событие пропущено.")
	case <-sendTimeout.C:
		log.Printf("⚠️ Буфер событий переполнен (таймаут отправки), событие %s для %s пропущено!", e.Type, e.Device.SerialNumber)
	}
}

func (m *Monitor) initialScan() error {
	log.Println("🔍 Начальное сканирование USB-устройств (system_profiler)...")
	devices := m.fetchCurrentUSBDevices() // Эта функция уже использует m.ctx

	m.devicesMutex.Lock() // Блокируем для безопасной записи логов о найденных
	defer m.devicesMutex.Unlock()

	count := 0
	for _, dev := range devices {
		if dev.IsDFU && dev.SerialNumber != "" {
			log.Printf("📱 Найдено при запуске (DFU/Recovery): %s (%s) - %s, ECID: %s",
				dev.SerialNumber, dev.Model, dev.State, dev.ECID)
			count++
		}
	}
	log.Printf("✅ Начальное сканирование (system_profiler): %d DFU/Recovery устройств обнаружено.", count)
	return nil
}

func (m *Monitor) GetConnectedDevices() []*Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	connected := make([]*Device, 0, len(m.devices))
	for _, dev := range m.devices {
		dCopy := *dev
		connected = append(connected, &dCopy)
	}
	return connected
}
