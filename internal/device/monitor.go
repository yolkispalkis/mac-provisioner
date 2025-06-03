package device

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
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
	cooldownChecker   func(string) (bool, time.Duration, string) // Новый чекер для периода охлаждения
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
		config:    cfg,
		events:    make(chan Event, 100),
		devices:   make(map[string]*Device),
		firstScan: true,
	}
}

func (m *Monitor) SetDFUTrigger(triggerFunc func(context.Context)) {
	m.dfuTriggerFunc = triggerFunc
}

func (m *Monitor) SetProcessingChecker(checker func(string) bool) {
	m.processingChecker = checker
}

// SetCooldownChecker устанавливает функцию для проверки периода охлаждения
func (m *Monitor) SetCooldownChecker(checker func(string) (bool, time.Duration, string)) {
	m.cooldownChecker = checker
}

func (m *Monitor) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

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

// checkAndTriggerDFU проверяет наличие устройств на DFU-портах и запускает macvdmtool
func (m *Monitor) checkAndTriggerDFU() {
	if m.dfuTriggerFunc == nil {
		return
	}

	m.mutex.RLock()
	defer m.mutex.RUnlock()

	needsDFUTrigger := false

	for _, dev := range m.devices {
		// Проверяем только обычные Mac и Recovery устройства на DFU-портах
		if (dev.IsNormalMac() || (dev.IsDFU && dev.State == "Recovery")) && m.isDFUPort(dev.USBLocation) {
			// Проверяем, не обрабатывается ли уже это устройство
			if m.processingChecker != nil && m.processingChecker(dev.USBLocation) {
				log.Printf("ℹ️ Устройство %s уже обрабатывается, пропускаем DFU триггер", dev.Name)
				continue
			}

			// Проверяем период охлаждения
			if m.cooldownChecker != nil {
				inCooldown, remaining, lastDevice := m.cooldownChecker(dev.USBLocation)
				if inCooldown {
					log.Printf("🕒 Порт %s в периоде охлаждения (осталось %v, последнее устройство: %s), пропускаем DFU триггер",
						dev.USBLocation, remaining.Round(time.Minute), lastDevice)
					continue
				}
			}

			log.Printf("🔄 Обнаружено устройство на DFU-порту: %s (%s)", dev.Name, dev.USBLocation)
			needsDFUTrigger = true
			break
		}
	}

	if needsDFUTrigger {
		log.Printf("⚡ Запускаем автоматический DFU триггер...")
		go m.dfuTriggerFunc(m.ctx)
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
		m.devices = currentMap
		for _, dev := range current {
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	for key, dev := range currentMap {
		if old, exists := m.devices[key]; !exists {
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if m.hasStateChanged(old, dev) {
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	for key, dev := range m.devices {
		if _, exists := currentMap[key]; !exists {
			delete(m.devices, key)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
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
		log.Printf("⚠️ Ошибка выполнения system_profiler: %v", err)
		return nil
	}

	var data USBData
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		log.Printf("⚠️ Ошибка парсинга JSON: %v", err)
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
			log.Printf("⚠️ DFU устройство без ECID игнорируется: %s", item.Name)
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
		log.Printf("⚠️ Буфер событий переполнен, событие пропущено: %s для %s", event.Type, event.Device.Name)
	}
}
