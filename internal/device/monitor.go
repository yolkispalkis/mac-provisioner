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

// Константы для Apple устройств
const (
	appleVendorIDHex    = "0x05ac"
	appleVendorIDString = "apple_vendor_id"
	appleManufacturer   = "Apple Inc."

	// DFU/Recovery режимы (известные PID)
	dfuModePIDAS      = "0x1281"
	recoveryModePIDAS = "0x1280"
	dfuModePIDIntelT2 = "0x1227"
)

type SPUSBItem struct {
	Name         string      `json:"_name"`
	ProductID    string      `json:"product_id,omitempty"`
	VendorID     string      `json:"vendor_id,omitempty"`
	SerialNum    string      `json:"serial_num,omitempty"`
	LocationID   string      `json:"location_id,omitempty"`
	Manufacturer string      `json:"manufacturer,omitempty"`
	SubItems     []SPUSBItem `json:"_items,omitempty"`
}

type SPUSBDataType struct {
	Items []SPUSBItem `json:"SPUSBDataType"`
}

// Вспомогательная функция для извлечения ECID
func extractECIDFromString(s string) string {
	marker := "ECID:"
	index := strings.Index(s, marker)
	if index == -1 {
		return ""
	}
	sub := s[index+len(marker):]
	endIndex := strings.Index(sub, " ")
	if endIndex == -1 {
		return strings.TrimSpace(sub)
	}
	return strings.TrimSpace(sub[:endIndex])
}

// Проверяет, является ли устройство Apple устройством
func isAppleDevice(item *SPUSBItem) bool {
	return strings.EqualFold(item.VendorID, appleVendorIDHex) ||
		strings.EqualFold(item.VendorID, appleVendorIDString) ||
		strings.Contains(item.Manufacturer, appleManufacturer)
}

// Проверяет, является ли устройство DFU/Recovery по PID
func isDFURecoveryByPID(productID string) (bool, string, string) {
	pidLower := strings.ToLower(productID)
	switch pidLower {
	case dfuModePIDAS:
		return true, "DFU", "Apple Silicon (DFU Mode)"
	case recoveryModePIDAS:
		return true, "Recovery", "Apple Silicon (Recovery Mode)"
	case dfuModePIDIntelT2:
		return true, "DFU", "Intel T2 (DFU Mode)"
	}
	return false, "", ""
}

// Проверяет, является ли устройство DFU/Recovery по имени
func isDFURecoveryByName(name string) (bool, string) {
	nameLower := strings.ToLower(name)
	if strings.Contains(nameLower, "dfu mode") {
		return true, "DFU"
	}
	if strings.Contains(nameLower, "recovery mode") {
		return true, "Recovery"
	}
	return false, ""
}

// Проверяет, является ли серийный номер ECID-ом
func isECIDFormat(serialNum string) bool {
	if serialNum == "" {
		return false
	}
	// ECID обычно содержит "ECID:" или выглядит как hex
	if strings.Contains(serialNum, "ECID:") {
		return true
	}
	// Проверяем, что это не обычный серийный номер Mac
	// Обычные серийные номера Mac имеют формат типа "00008103-000C599E36BB001E"
	if strings.Contains(serialNum, "-") && len(serialNum) > 15 {
		return false
	}
	return false
}

// Проверяет, является ли это обычным Mac устройством
func isNormalMacDevice(item *SPUSBItem) bool {
	if !isAppleDevice(item) {
		return false
	}

	// Если это DFU/Recovery - не обычный Mac
	if isDFU, _, _ := isDFURecoveryByPID(item.ProductID); isDFU {
		return false
	}
	if isDFU, _ := isDFURecoveryByName(item.Name); isDFU {
		return false
	}

	// Проверяем по имени устройства
	nameLower := strings.ToLower(item.Name)
	macKeywords := []string{"macbook", "imac", "mac mini", "mac studio", "mac pro"}
	for _, keyword := range macKeywords {
		if strings.Contains(nameLower, keyword) {
			return true
		}
	}

	// Если есть серийный номер в формате Mac и это Apple устройство
	if item.SerialNum != "" &&
		item.SerialNum != "N/A" &&
		!isECIDFormat(item.SerialNum) &&
		isAppleDevice(item) {
		return true
	}

	return false
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}
	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB устройств (через system_profiler)...")

	if err := m.initialScan(); err != nil {
		log.Printf("⚠️ Начальное сканирование (system_profiler): %v", err)
	}

	go m.monitorLoop()
	go m.cleanupLoop()

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
}

func (m *Monitor) Events() <-chan Event { return m.eventChan }

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
			log.Printf("Предупреждение: Обнаружено устройство без серийного номера: %s", dev.Model)
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
			if oldDev.State != currentDev.State || oldDev.IsDFU != currentDev.IsDFU || oldDev.USBLocation != currentDev.USBLocation || oldDev.ECID != currentDev.ECID {
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
	// Проверяем, является ли это Apple устройством
	if !isAppleDevice(spItem) {
		// Рекурсивно проверяем подэлементы
		if spItem.SubItems != nil {
			for i := range spItem.SubItems {
				m.extractDevicesRecursively(&spItem.SubItems[i], devices)
			}
		}
		return
	}

	// Это Apple устройство, определяем тип
	var dev *Device

	// Сначала проверяем DFU/Recovery по PID
	if isDFU, state, model := isDFURecoveryByPID(spItem.ProductID); isDFU {
		dev = &Device{
			Model:       model,
			State:       state,
			IsDFU:       true,
			USBLocation: spItem.LocationID,
		}

		// Для DFU устройств извлекаем ECID
		parsedECID := extractECIDFromString(spItem.SerialNum)
		if parsedECID != "" {
			dev.ECID = parsedECID
			dev.SerialNumber = "DFU-" + strings.ToLower(dev.ECID)
		} else {
			log.Printf("⚠️ DFU/Recovery device (%s) - ECID not found in serial_num: '%s'. Device will be ignored.", model, spItem.SerialNum)
			dev = nil // Игнорируем DFU без ECID
		}
	} else if isDFU, state := isDFURecoveryByName(spItem.Name); isDFU {
		// Проверяем DFU/Recovery по имени (fallback)
		dev = &Device{
			Model:       spItem.Name,
			State:       state,
			IsDFU:       true,
			USBLocation: spItem.LocationID,
		}

		parsedECID := extractECIDFromString(spItem.SerialNum)
		if parsedECID != "" {
			dev.ECID = parsedECID
			dev.SerialNumber = "DFU-" + strings.ToLower(dev.ECID)
		} else {
			log.Printf("⚠️ DFU/Recovery device (%s) - ECID not found in serial_num: '%s'. Device will be ignored.", spItem.Name, spItem.SerialNum)
			dev = nil
		}
	} else if isNormalMacDevice(spItem) {
		// Это обычный Mac
		dev = &Device{
			Model:        spItem.Name,
			State:        "Normal",
			IsDFU:        false,
			SerialNumber: spItem.SerialNum,
			USBLocation:  spItem.LocationID,
		}
	}

	// Добавляем устройство, если оно валидно
	if dev != nil && dev.IsValidSerial() {
		*devices = append(*devices, dev)
		log.Printf("🔍 Обнаружено устройство: %s (SN: %s, DFU: %v, State: %s)",
			dev.Model, dev.SerialNumber, dev.IsDFU, dev.State)
	}

	// Рекурсивно проверяем подэлементы
	if spItem.SubItems != nil {
		for i := range spItem.SubItems {
			m.extractDevicesRecursively(&spItem.SubItems[i], devices)
		}
	}
}

func (m *Monitor) sendEvent(e Event) {
	sendTimeout := time.NewTimer(100 * time.Millisecond)
	defer sendTimeout.Stop()

	select {
	case m.eventChan <- e:
	case <-m.ctx.Done():
		log.Println("ℹ️ Канал событий не принимает (контекст завершен), событие пропущено.")
	case <-sendTimeout.C:
		log.Printf("⚠️ Буфер событий переполнен (таймаут отправки), событие %s для %s пропущено!", e.Type, e.Device.SerialNumber)
	}
}

func (m *Monitor) initialScan() error {
	log.Println("🔍 Начальное сканирование USB-устройств (system_profiler)...")
	devices := m.fetchCurrentUSBDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	dfuCount := 0
	normalCount := 0
	for _, dev := range devices {
		if dev.IsDFU && dev.SerialNumber != "" {
			log.Printf("📱 Найдено при запуске (DFU/Recovery): %s (%s) - %s, ECID: %s",
				dev.SerialNumber, dev.Model, dev.State, dev.ECID)
			dfuCount++
		} else if !dev.IsDFU && dev.SerialNumber != "" {
			log.Printf("💻 Найдено при запуске (Normal Mac): %s (%s) - %s",
				dev.SerialNumber, dev.Model, dev.State)
			normalCount++
		}
	}
	log.Printf("✅ Начальное сканирование (system_profiler): %d DFU/Recovery + %d обычных Mac устройств обнаружено.", dfuCount, normalCount)
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
