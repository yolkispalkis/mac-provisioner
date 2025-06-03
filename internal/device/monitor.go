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
		currentMap[dev.UniqueID()] = dev
	}

	if m.firstScan {
		m.devices = currentMap
		for _, dev := range current {
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	// Новые устройства
	for id, dev := range currentMap {
		if old, exists := m.devices[id]; !exists {
			m.devices[id] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if old.State != dev.State {
			m.devices[id] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	// Отключенные устройства
	for id, dev := range m.devices {
		if _, exists := currentMap[id]; !exists {
			delete(m.devices, id)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
		}
	}
}

func (m *Monitor) getCurrentDevices() []*Device {
	cmd := exec.CommandContext(m.ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return nil
	}

	var data USBData
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
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
		if strings.Contains(strings.ToLower(item.Name), "dfu") {
			dev.State = "DFU"
		} else {
			dev.State = "Recovery"
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
		item.ProductID == "0x1281" || // DFU Mode
		item.ProductID == "0x1280" // Recovery Mode
}

func (m *Monitor) isNormalMac(item *USBDevice) bool {
	name := strings.ToLower(item.Name)
	return strings.Contains(name, "macbook") ||
		strings.Contains(name, "imac") ||
		strings.Contains(name, "mac mini") ||
		strings.Contains(name, "mac studio") ||
		strings.Contains(name, "mac pro")
}

func (m *Monitor) extractECID(serial string) string {
	re := regexp.MustCompile(`(?i)ecid[:\s]*([0-9a-fx]+)`)
	if matches := re.FindStringSubmatch(serial); len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

func (m *Monitor) sendEvent(event Event) {
	select {
	case m.events <- event:
	default:
		log.Printf("⚠️ Буфер событий переполнен")
	}
}
