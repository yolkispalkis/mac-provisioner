package device

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"regexp"
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
	config            config.MonitoringConfig
	events            chan Event
	devices           map[string]*Device
	mutex             sync.RWMutex
	ctx               context.Context
	cancel            context.CancelFunc
	firstScan         bool
	dfuTriggerFunc    func(context.Context)
	processingChecker func(string) bool
	cooldownChecker   func(string) (bool, string)
	debugMode         bool
	deviceResolver    *DeviceResolver
}

type USBDevice struct {
	Name         string      `json:"_name"`
	ProductID    string      `json:"product_id"`
	VendorID     string      `json:"vendor_id"`
	SerialNum    string      `json:"serial_num"`
	LocationID   string      `json:"location_id"`
	Manufacturer string      `json:"manufacturer"`
	Items        []USBDevice `json:"_items"`
}

type USBData struct {
	USB []USBDevice `json:"SPUSBDataType"`
}

func NewMonitor(cfg config.MonitoringConfig) *Monitor {
	return &Monitor{
		config:         cfg,
		events:         make(chan Event, 100),
		devices:        make(map[string]*Device),
		firstScan:      true,
		debugMode:      os.Getenv("MAC_PROV_DEBUG") == "1",
		deviceResolver: NewDeviceResolver(),
	}
}

func (m *Monitor) SetDFUTrigger(triggerFunc func(context.Context)) {
	m.dfuTriggerFunc = triggerFunc
}

func (m *Monitor) SetProcessingChecker(checker func(string) bool) {
	m.processingChecker = checker
}

func (m *Monitor) SetCooldownChecker(checker func(string) (bool, string)) {
	m.cooldownChecker = checker
}

func (m *Monitor) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// Запускаем очистку кэша имен устройств
	go m.deviceResolver.StartCleanupRoutine(ctx)

	m.scanDevices()

	go m.monitorLoop()
	return nil
}

func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Monitor) Events() <-chan Event {
	return m.events
}

func (m *Monitor) monitorLoop() {
	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.scanDevices()
			m.checkAndTriggerDFU()
		}
	}
}

// checkAndTriggerDFU с исправленной логикой
func (m *Monitor) checkAndTriggerDFU() {
	if m.dfuTriggerFunc == nil {
		return
	}

	m.mutex.RLock()
	defer m.mutex.RUnlock()

	// Ищем все DFU-порты и определяем, какие устройства на них подключены
	dfuPorts := make(map[string]*Device) // key = USB location, value = device (или nil если порт пустой)

	// Находим все устройства на DFU-портах
	for _, dev := range m.devices {
		if m.isDFUPort(dev.USBLocation) {
			dfuPorts[dev.USBLocation] = dev
		}
	}

	// Проверяем каждый найденный DFU-порт
	for usbLocation, dev := range dfuPorts {
		// Пропускаем порты, где идет обработка
		if m.processingChecker != nil && m.processingChecker(usbLocation) {
			if m.debugMode {
				log.Printf("🔍 [DEBUG] Порт %s занят обработкой %s", usbLocation, dev.GetDisplayName())
			}
			continue
		}

		var deviceECID string
		var deviceName string

		if dev != nil {
			deviceECID = dev.ECID
			deviceName = dev.GetDisplayName()

			// Проверяем только устройства, которые нужно переводить в DFU
			// НЕ запускаем DFU для устройств уже в DFU режиме!
			if dev.IsDFU {
				if m.debugMode {
					log.Printf("🔍 [DEBUG] Пропускаем DFU для %s - устройство уже в DFU/Recovery режиме", deviceName)
				}
				continue
			}

			if !dev.IsNormalMac() {
				if m.debugMode {
					log.Printf("🔍 [DEBUG] Пропускаем DFU для %s - не обычный Mac", deviceName)
				}
				continue
			}
		}

		// Проверяем, нужно ли запускать DFU
		if m.cooldownChecker != nil {
			shouldTrigger, reason := m.cooldownChecker(deviceECID)

			if shouldTrigger {
				if dev != nil {
					if m.debugMode {
						log.Printf("🔍 [DEBUG] Запускаем DFU для %s на порту %s: %s",
							deviceName, usbLocation, reason)
					}
					log.Printf("⚡ Запуск автоматического DFU для %s", deviceName)
				} else {
					// Этот случай не должен происходить, так как мы итерируемся только по найденным устройствам
					if m.debugMode {
						log.Printf("🔍 [DEBUG] Неожиданная ситуация: пустой порт %s в списке устройств", usbLocation)
					}
				}
				go m.dfuTriggerFunc(m.ctx)
				return // Запускаем DFU только для одного порта за раз
			} else {
				if m.debugMode && dev != nil {
					log.Printf("🔍 [DEBUG] Пропускаем DFU для %s: %s", deviceName, reason)
				}
			}
		}
	}

	// Если нет устройств на DFU-портах вообще, запускаем DFU для пустого порта
	if len(dfuPorts) == 0 {
		// Проверяем, что нет активной обработки на основном порту
		mainPort := "0x00100000/1"
		if m.processingChecker != nil && m.processingChecker(mainPort) {
			if m.debugMode {
				log.Printf("🔍 [DEBUG] Основной порт %s занят обработкой", mainPort)
			}
			return
		}

		if m.cooldownChecker != nil {
			shouldTrigger, reason := m.cooldownChecker("")
			if shouldTrigger {
				if m.debugMode {
					log.Printf("🔍 [DEBUG] Запускаем DFU для пустого порта %s: %s", mainPort, reason)
				}
				log.Printf("⚡ Запуск автоматического DFU (порт пустой)")
				go m.dfuTriggerFunc(m.ctx)
			} else {
				if m.debugMode {
					log.Printf("🔍 [DEBUG] Пропускаем DFU для пустого порта: %s", reason)
				}
			}
		}
	}
}

func (m *Monitor) isDFUPort(usbLocation string) bool {
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

func (m *Monitor) scanDevices() {
	current := m.getCurrentDevices()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	currentMap := make(map[string]*Device)
	for _, dev := range current {
		key := m.getDeviceKey(dev)
		currentMap[key] = dev
	}

	if m.firstScan {
		// Для первого сканирования сначала получаем имена, потом отправляем события
		m.resolveDeviceNamesSync(current)
		m.devices = currentMap
		for _, dev := range current {
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	for key, dev := range currentMap {
		if old, exists := m.devices[key]; !exists {
			// Новое устройство - сначала получаем имя, потом отправляем событие
			if dev.IsDFU && dev.ECID != "" {
				m.resolveDeviceNameSync(dev)
			}
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if m.hasStateChanged(old, dev) {
			// Сохраняем resolved name из старого устройства
			if old.ResolvedName != "" {
				dev.ResolvedName = old.ResolvedName
			}
			// Если это переход в DFU и у нас еще нет красивого имени - получаем его
			if dev.IsDFU && dev.ECID != "" && dev.ResolvedName == "" {
				m.resolveDeviceNameSync(dev)
			}
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		} else {
			// Обновляем resolved name если он появился
			if old.ResolvedName != "" && dev.ResolvedName == "" {
				dev.ResolvedName = old.ResolvedName
			}
			m.devices[key] = dev
		}
	}

	for key, dev := range m.devices {
		if _, exists := currentMap[key]; !exists {
			delete(m.devices, key)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
		}
	}
}

// resolveDeviceNamesSync синхронно получает имена для всех DFU устройств
func (m *Monitor) resolveDeviceNamesSync(devices []*Device) {
	var dfuDevices []*Device
	for _, dev := range devices {
		if dev.IsDFU && dev.ECID != "" {
			dfuDevices = append(dfuDevices, dev)
		}
	}

	if len(dfuDevices) == 0 {
		return
	}

	if m.debugMode {
		log.Printf("🔍 [DEBUG] Получаем имена для %d DFU устройств...", len(dfuDevices))
	}

	// Получаем имена синхронно с таймаутом
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	for _, dev := range dfuDevices {
		resolvedName := m.deviceResolver.ResolveDeviceNameSync(ctx, dev.ECID, dev.Name)
		if resolvedName != dev.Name {
			dev.ResolvedName = resolvedName
			if m.debugMode {
				log.Printf("🔍 [DEBUG] Получено имя: %s -> %s", dev.ECID, resolvedName)
			}
		}
	}
}

// resolveDeviceNameSync синхронно получает имя для одного устройства
func (m *Monitor) resolveDeviceNameSync(dev *Device) {
	if dev.ECID == "" {
		return
	}

	if m.debugMode {
		log.Printf("🔍 [DEBUG] Получаем имя для устройства %s...", dev.ECID)
	}

	// Получаем имя синхронно с таймаутом
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	resolvedName := m.deviceResolver.ResolveDeviceNameSync(ctx, dev.ECID, dev.Name)
	if resolvedName != dev.Name {
		dev.ResolvedName = resolvedName
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Получено имя: %s -> %s", dev.ECID, resolvedName)
		}
	}
}

func (m *Monitor) getDeviceKey(dev *Device) string {
	if dev.ECID != "" {
		return "ecid:" + dev.ECID + ":state:" + dev.State
	}
	return "usb:" + dev.USBLocation
}

func (m *Monitor) hasStateChanged(old, new *Device) bool {
	return old.State != new.State ||
		old.IsDFU != new.IsDFU ||
		old.Name != new.Name ||
		old.ECID != new.ECID
}

func (m *Monitor) getCurrentDevices() []*Device {
	cmd := exec.CommandContext(m.ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Ошибка выполнения system_profiler: %v", err)
		}
		return nil
	}

	var data USBData
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Ошибка парсинга JSON: %v", err)
		}
		return nil
	}

	var devices []*Device
	for _, item := range data.USB {
		m.extractDevices(&item, &devices)
	}

	return devices
}

func (m *Monitor) extractDevices(item *USBDevice, devices *[]*Device) {
	if m.isAppleDevice(item) {
		if dev := m.createDevice(item); dev != nil {
			*devices = append(*devices, dev)
		}
	}

	for i := range item.Items {
		m.extractDevices(&item.Items[i], devices)
	}
}

func (m *Monitor) isAppleDevice(item *USBDevice) bool {
	return strings.EqualFold(item.VendorID, "0x05ac") ||
		strings.Contains(item.Manufacturer, "Apple")
}

func (m *Monitor) createDevice(item *USBDevice) *Device {
	dev := &Device{
		Name:        item.Name,
		USBLocation: item.LocationID,
	}

	if m.isDFUDevice(item) {
		dev.IsDFU = true
		dev.ECID = m.extractECID(item.SerialNum)

		if strings.Contains(strings.ToLower(item.Name), "dfu") || item.ProductID == "0x1281" || item.ProductID == "0x1227" {
			dev.State = "DFU"
		} else {
			dev.State = "Recovery"
		}

		if dev.ECID == "" {
			if m.debugMode {
				log.Printf("🔍 [DEBUG] DFU устройство без ECID игнорируется: %s", item.Name)
			}
			return nil
		}
	} else if m.isNormalMac(item) {
		dev.IsDFU = false
		dev.State = "Normal"
	} else {
		return nil
	}

	return dev
}

func (m *Monitor) isDFUDevice(item *USBDevice) bool {
	name := strings.ToLower(item.Name)
	return strings.Contains(name, "dfu mode") ||
		strings.Contains(name, "recovery mode") ||
		item.ProductID == "0x1281" ||
		item.ProductID == "0x1280" ||
		item.ProductID == "0x1227"
}

func (m *Monitor) isNormalMac(item *USBDevice) bool {
	name := strings.ToLower(item.Name)
	macKeywords := []string{"macbook", "imac", "mac mini", "mac studio", "mac pro"}

	for _, keyword := range macKeywords {
		if strings.Contains(name, keyword) {
			return true
		}
	}

	return item.SerialNum != "" && len(item.SerialNum) > 0
}

func (m *Monitor) extractECID(serial string) string {
	if serial == "" {
		return ""
	}

	re := regexp.MustCompile(`(?i)ecid[:\s]*([0-9a-fx]+)`)
	if matches := re.FindStringSubmatch(serial); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}

	trimmed := strings.TrimSpace(serial)
	if m.isValidECID(trimmed) {
		return trimmed
	}

	return ""
}

func (m *Monitor) isValidECID(s string) bool {
	if s == "" {
		return false
	}

	if strings.HasPrefix(strings.ToLower(s), "0x") {
		hex := s[2:]
		if len(hex) > 0 && len(hex) <= 16 {
			for _, r := range hex {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
					return false
				}
			}
			return true
		}
	}

	if len(s) > 0 && len(s) <= 20 {
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}

	return false
}

func (m *Monitor) sendEvent(event Event) {
	select {
	case m.events <- event:
	case <-time.After(100 * time.Millisecond):
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Буфер событий переполнен, событие пропущено: %s для %s", event.Type, event.Device.GetDisplayName())
		}
	}
}

// GetDeviceResolver возвращает resolver для использования в других компонентах
func (m *Monitor) GetDeviceResolver() *DeviceResolver {
	return m.deviceResolver
}
