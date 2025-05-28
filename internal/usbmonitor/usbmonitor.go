package usbmonitor

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/device"
)

type USBEvent struct {
	Type   string         `json:"type"` // "connected", "disconnected", "state_changed"
	Device *device.Device `json:"device"`
}

type Monitor struct {
	eventChan    chan USBEvent
	stopChan     chan struct{}
	devices      map[string]*device.Device
	devicesMutex sync.RWMutex
	running      bool
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewMonitor() *Monitor {
	return &Monitor{
		eventChan: make(chan USBEvent, 100),
		stopChan:  make(chan struct{}),
		devices:   make(map[string]*device.Device),
	}
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("monitor already running")
	}

	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("Starting simplified USB monitor...")

	// Получаем начальное состояние устройств
	if err := m.initialScan(); err != nil {
		log.Printf("Warning: Initial scan failed: %v", err)
	}

	// Запускаем быстрый мониторинг
	go m.monitorUSBChanges()

	// Запускаем мониторинг состояний через cfgutil
	go m.monitorDeviceStates()

	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}

	log.Println("Stopping USB monitor...")
	m.running = false

	if m.cancel != nil {
		m.cancel()
	}

	close(m.stopChan)
}

func (m *Monitor) Events() <-chan USBEvent {
	return m.eventChan
}

func (m *Monitor) monitorUSBChanges() {
	ticker := time.NewTicker(500 * time.Millisecond) // Быстрая проверка для обнаружения подключений
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkUSBDevices()
		}
	}
}

func (m *Monitor) monitorDeviceStates() {
	ticker := time.NewTicker(2 * time.Second) // Проверяем состояния каждые 2 секунды
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkDeviceStates()
		}
	}
}

func (m *Monitor) checkUSBDevices() {
	// Получаем cfgutil устройства
	cfgutilDevices, err := m.getCfgutilDevices()
	if err != nil {
		return
	}

	// Получаем USB устройства через system_profiler (для DFU устройств)
	usbDevices, _ := m.getUSBDevices()

	// Объединяем списки
	allDevices := append(cfgutilDevices, usbDevices...)
	allDevices = m.removeDuplicates(allDevices)

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// Создаем карту текущих устройств
	currentMap := make(map[string]*device.Device)
	for _, dev := range allDevices {
		currentMap[dev.SerialNumber] = dev
	}

	// Проверяем новые устройства
	for serial, dev := range currentMap {
		if _, exists := m.devices[serial]; !exists {
			m.devices[serial] = dev
			select {
			case m.eventChan <- USBEvent{Type: "connected", Device: dev}:
			default:
				log.Println("Event channel full, dropping connect event")
			}
		}
	}

	// Проверяем отключенные устройства
	for serial, dev := range m.devices {
		if _, exists := currentMap[serial]; !exists {
			delete(m.devices, serial)
			select {
			case m.eventChan <- USBEvent{Type: "disconnected", Device: dev}:
			default:
				log.Println("Event channel full, dropping disconnect event")
			}
		}
	}
}

func (m *Monitor) checkDeviceStates() {
	cfgutilDevices, err := m.getCfgutilDevices()
	if err != nil {
		return
	}

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// Проверяем изменения состояния
	for _, currentDev := range cfgutilDevices {
		if existingDev, exists := m.devices[currentDev.SerialNumber]; exists {
			if existingDev.State != currentDev.State || existingDev.IsDFU != currentDev.IsDFU {
				m.devices[currentDev.SerialNumber] = currentDev
				select {
				case m.eventChan <- USBEvent{Type: "state_changed", Device: currentDev}:
				default:
					log.Println("Event channel full, dropping state change event")
				}
			}
		}
	}
}

func (m *Monitor) getUSBDevices() ([]*device.Device, error) {
	cmd := exec.Command("system_profiler", "SPUSBDataType", "-detailLevel", "mini")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return m.parseSystemProfilerOutput(string(output)), nil
}

func (m *Monitor) parseSystemProfilerOutput(output string) []*device.Device {
	var devices []*device.Device
	lines := strings.Split(output, "\n")

	var currentDevice *device.Device

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Ищем Apple устройства
		if strings.Contains(line, ":") && m.isAppleDeviceLine(line) {
			if currentDevice != nil && currentDevice.SerialNumber != "" {
				devices = append(devices, currentDevice)
			}

			deviceName := strings.Split(line, ":")[0]
			currentDevice = &device.Device{
				Model: deviceName,
				State: "connected",
				IsDFU: m.isDFUDeviceName(deviceName),
			}
		}

		// Ищем серийный номер
		if currentDevice != nil && strings.Contains(line, "Serial Number:") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				serial := strings.TrimSpace(parts[1])
				if serial != "" && serial != "N/A" {
					currentDevice.SerialNumber = serial
				}
			}
		}
	}

	// Добавляем последнее устройство
	if currentDevice != nil && currentDevice.SerialNumber != "" {
		devices = append(devices, currentDevice)
	}

	return devices
}

func (m *Monitor) isAppleDeviceLine(line string) bool {
	line = strings.ToLower(line)
	keywords := []string{
		"macbook", "imac", "mac mini", "mac studio", "mac pro",
		"apple t2", "apple t1", "dfu", "recovery",
		"apple mobile device", "apple configurator",
	}

	for _, keyword := range keywords {
		if strings.Contains(line, keyword) {
			return true
		}
	}

	return false
}

func (m *Monitor) isDFUDeviceName(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "dfu") || strings.Contains(name, "recovery")
}

func (m *Monitor) getCfgutilDevices() ([]*device.Device, error) {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return m.parseCfgutilOutput(string(output)), nil
}

func (m *Monitor) parseCfgutilOutput(output string) []*device.Device {
	var devices []*device.Device
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ECID") || strings.HasPrefix(line, "Name") {
			continue
		}

		device := m.parseDeviceLine(line)
		if device != nil {
			devices = append(devices, device)
		}
	}

	return devices
}

func (m *Monitor) parseDeviceLine(line string) *device.Device {
	device := &device.Device{
		IsDFU: false,
	}

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

func (m *Monitor) removeDuplicates(devices []*device.Device) []*device.Device {
	seen := make(map[string]bool)
	var result []*device.Device

	for _, device := range devices {
		if device.SerialNumber != "" && !seen[device.SerialNumber] {
			seen[device.SerialNumber] = true
			result = append(result, device)
		}
	}

	return result
}

func (m *Monitor) initialScan() error {
	devices, err := m.getCurrentDevices()
	if err != nil {
		return err
	}

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	for _, dev := range devices {
		m.devices[dev.SerialNumber] = dev
	}

	log.Printf("Initial scan found %d devices", len(devices))
	return nil
}

func (m *Monitor) getCurrentDevices() ([]*device.Device, error) {
	// Получаем устройства из обоих источников
	cfgutilDevices, _ := m.getCfgutilDevices()
	usbDevices, _ := m.getUSBDevices()

	allDevices := append(cfgutilDevices, usbDevices...)
	return m.removeDuplicates(allDevices), nil
}

func (m *Monitor) GetConnectedDevices() []*device.Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	var devices []*device.Device
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}

	return devices
}
