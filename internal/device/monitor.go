package device

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	config    config.MonitoringConfig
	events    chan Event
	devices   map[string]*Device
	mutex     sync.RWMutex
	ctx       context.Context
	cancel    context.CancelFunc
	firstScan bool
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

func (m *Monitor) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)

	// Выполняем первоначальное сканирование
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
		}
	}
}

func (m *Monitor) scanDevices() {
	current := m.getCurrentDevices()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	currentMap := make(map[string]*Device)
	for _, dev := range current {
		// Используем комбинированный ключ для более точной идентификации
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

	// Новые устройства и изменения состояния
	for key, dev := range currentMap {
		if old, exists := m.devices[key]; !exists {
			// Новое устройство
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if m.hasStateChanged(old, dev) {
			// Изменение состояния
			m.devices[key] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	// Отключенные устройства
	for key, dev := range m.devices {
		if _, exists := currentMap[key]; !exists {
			delete(m.devices, key)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
		}
	}
}

// getDeviceKey создает уникальный ключ для устройства
func (m *Monitor) getDeviceKey(dev *Device) string {
	if dev.ECID != "" {
		// Для DFU устройств используем ECID + состояние
		return fmt.Sprintf("ecid:%s:state:%s", dev.ECID, dev.State)
	}
	// Для обычных устройств используем USB location
	return fmt.Sprintf("usb:%s", dev.USBLocation)
}

// hasStateChanged проверяет, изменилось ли состояние устройства
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

	// Определение типа устройства
	if m.isDFUDevice(item) {
		dev.IsDFU = true
		dev.ECID = m.extractECID(item.SerialNum)

		// Определяем состояние по имени или Product ID
		if strings.Contains(strings.ToLower(item.Name), "dfu") || item.ProductID == "0x1281" || item.ProductID == "0x1227" {
			dev.State = "DFU"
		} else {
			dev.State = "Recovery"
		}

		// Для DFU устройств ECID обязателен
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
		item.ProductID == "0x1281" || // Apple Silicon DFU Mode
		item.ProductID == "0x1280" || // Apple Silicon Recovery Mode
		item.ProductID == "0x1227" // Intel T2 DFU Mode
}

func (m *Monitor) isNormalMac(item *USBDevice) bool {
	name := strings.ToLower(item.Name)
	macKeywords := []string{"macbook", "imac", "mac mini", "mac studio", "mac pro"}

	for _, keyword := range macKeywords {
		if strings.Contains(name, keyword) {
			return true
		}
	}

	// Дополнительная проверка для Apple устройств с серийным номером
	return item.SerialNum != "" && len(item.SerialNum) > 0
}

func (m *Monitor) extractECID(serial string) string {
	if serial == "" {
		return ""
	}

	// Ищем ECID в формате "ECID: значение"
	re := regexp.MustCompile(`(?i)ecid[:\s]*([0-9a-fx]+)`)
	if matches := re.FindStringSubmatch(serial); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}

	// Проверяем, является ли вся строка ECID (hex или decimal)
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

	// Проверяем hex формат
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

	// Проверяем decimal формат
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
