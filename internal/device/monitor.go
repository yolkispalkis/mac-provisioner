package device

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	devices      map[string]*Device // key = Device.UniqueID()
	devicesMutex sync.RWMutex
	running      bool
	ctx          context.Context
	cancel       context.CancelFunc
	firstScan    bool

	nameResolver *NameResolver // 🔄  новое поле
}

func NewMonitor(cfg config.MonitoringConfig) *Monitor {
	return &Monitor{
		config:       cfg,
		eventChan:    make(chan Event, cfg.EventBufferSize),
		devices:      make(map[string]*Device),
		firstScan:    true,
		nameResolver: NewNameResolver(), // 🔄
	}
}

/*
──────────────────────────────────────────────────────────

	DEBUG helper (вкл. через env MAC_PROV_DEBUG=1)
	──────────────────────────────────────────────────────────
*/
var debugEnabled = os.Getenv("MAC_PROV_DEBUG") == "1"

func debugLog(format string, a ...interface{}) {
	if debugEnabled {
		log.Printf(format, a...)
	}
}

/*
──────────────────────────────────────────────────────────

	USB constants (общие с dfu/manager.go)
	──────────────────────────────────────────────────────────
*/
const (
	appleVendorIDHex    = "0x05ac"
	appleVendorIDString = "apple_vendor_id"
	appleManufacturer   = "Apple Inc."

	dfuModePIDAS      = "0x1281"
	recoveryModePIDAS = "0x1280"
	dfuModePIDIntelT2 = "0x1227"
)

/*
──────────────────────────────────────────────────────────

	system_profiler JSON types
	──────────────────────────────────────────────────────────
*/
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

/*
──────────────────────────────────────────────────────────

	Helpers (ECID, Mac detection, …)
	──────────────────────────────────────────────────────────
*/
func extractECIDFromString(s string) string {
	const marker = "ECID:"
	idx := strings.Index(s, marker)
	if idx == -1 {
		return ""
	}
	sub := s[idx+len(marker):]
	if end := strings.Index(sub, " "); end != -1 {
		sub = sub[:end]
	}
	return strings.TrimSpace(sub)
}

func isAppleDevice(item *SPUSBItem) bool {
	return strings.EqualFold(item.VendorID, appleVendorIDHex) ||
		strings.EqualFold(item.VendorID, appleVendorIDString) ||
		strings.Contains(item.Manufacturer, appleManufacturer)
}

func isDFURecoveryByPID(productID string) (bool, string, string) {
	switch strings.ToLower(productID) {
	case dfuModePIDAS:
		return true, "DFU", "Apple Silicon (DFU Mode)"
	case recoveryModePIDAS:
		return true, "Recovery", "Apple Silicon (Recovery Mode)"
	case dfuModePIDIntelT2:
		return true, "DFU", "Intel T2 (DFU Mode)"
	}
	return false, "", ""
}

func isDFURecoveryByName(name string) (bool, string) {
	l := strings.ToLower(name)
	if strings.Contains(l, "dfu mode") {
		return true, "DFU"
	}
	if strings.Contains(l, "recovery mode") {
		return true, "Recovery"
	}
	return false, ""
}

func isNormalMacDevice(item *SPUSBItem) bool {
	if !isAppleDevice(item) {
		return false
	}

	if isDFU, _, _ := isDFURecoveryByPID(item.ProductID); isDFU {
		return false
	}
	if isDFU, _ := isDFURecoveryByName(item.Name); isDFU {
		return false
	}

	nameLower := strings.ToLower(item.Name)
	for _, kw := range []string{"macbook", "imac", "mac mini", "mac studio", "mac pro"} {
		if strings.Contains(nameLower, kw) {
			return true
		}
	}
	return item.SerialNum != "" && isAppleDevice(item)
}

/*
──────────────────────────────────────────────────────────

	Public API
	──────────────────────────────────────────────────────────
*/
func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}
	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB-устройств (system_profiler)…")

	if err := m.initialScan(); err != nil {
		log.Printf("⚠️  Начальное сканирование: %v", err)
	}

	go m.monitorLoop()
	go m.cleanupLoop()

	log.Println("✅ Мониторинг USB-устройств запущен")
	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}
	log.Println("🛑 Остановка мониторинга USB")
	m.running = false
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *Monitor) Events() <-chan Event { return m.eventChan }

/*
──────────────────────────────────────────────────────────

	Goroutines
	──────────────────────────────────────────────────────────
*/
func (m *Monitor) monitorLoop() {
	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()
	log.Printf("🔄 Цикл мониторинга каждые %v", m.config.CheckInterval)

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
	t := time.NewTicker(m.config.CleanupInterval)
	defer t.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.devicesMutex.RLock()
			debugLog("🧹 Следим за %d устройствами", len(m.devices))
			m.devicesMutex.RUnlock()
		}
	}
}

/*
──────────────────────────────────────────────────────────

	Core
	──────────────────────────────────────────────────────────
*/
func (m *Monitor) checkDevices() {
	current := m.fetchCurrentUSBDevices()

	// 1) нормальные Маки → кэшируем имя по USB
	for _, dev := range current {
		if dev.IsNormalMac() {
			m.nameResolver.rememberNormal(dev)
		}
	}

	// 2) DFU → пытаемся «приукрасить» модель
	for _, dev := range current {
		if dev.IsDFU {
			_ = m.nameResolver.tryBeautifyDFU(m.ctx, dev)
		}
	}

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	nowMap := make(map[string]*Device, len(current))
	for _, dev := range current {
		if uid := dev.UniqueID(); uid != "" {
			nowMap[uid] = dev
		}
	}

	// первое сканирование
	if m.firstScan {
		for uid, dev := range nowMap {
			m.devices[uid] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	// new / changed
	for uid, cur := range nowMap {
		old, exists := m.devices[uid]
		if !exists {
			m.devices[uid] = cur
			m.sendEvent(Event{Type: EventConnected, Device: cur})
			continue
		}
		if old.State != cur.State || old.IsDFU != cur.IsDFU || old.ECID != cur.ECID {
			m.devices[uid] = cur
			m.sendEvent(Event{Type: EventStateChanged, Device: cur})
		}
	}

	// disconnected
	for uid, old := range m.devices {
		if _, ok := nowMap[uid]; !ok {
			delete(m.devices, uid)
			m.sendEvent(Event{Type: EventDisconnected, Device: old})
		}
	}
}

/*
──────────────────────────────────────────────────────────

	system_profiler parsing
	──────────────────────────────────────────────────────────
*/
func (m *Monitor) fetchCurrentUSBDevices() []*Device {
	cmd := exec.CommandContext(m.ctx, "system_profiler", "SPUSBDataType", "-json")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if m.ctx.Err() == context.Canceled {
			return nil
		}
		log.Printf("❌ system_profiler: %v – %s", err, stderr.String())
		return nil
	}

	var data SPUSBDataType
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		log.Printf("❌ JSON parse: %v", err)
		return nil
	}

	var list []*Device
	for i := range data.Items {
		m.extractDevicesRecursively(&data.Items[i], &list)
	}
	return list
}

func (m *Monitor) extractDevicesRecursively(sp *SPUSBItem, acc *[]*Device) {
	if !isAppleDevice(sp) {
		for i := range sp.SubItems {
			m.extractDevicesRecursively(&sp.SubItems[i], acc)
		}
		return
	}

	var dev *Device

	// DFU / Recovery
	if isDFU, state, model := isDFURecoveryByPID(sp.ProductID); isDFU {
		dev = &Device{
			Model:       model,
			State:       state,
			IsDFU:       true,
			USBLocation: sp.LocationID,
		}
		if ecid := extractECIDFromString(sp.SerialNum); ecid != "" {
			dev.ECID = ecid
		} else {
			debugLog("⚠️  DFU без ECID (%s) – игнор.", model)
			dev = nil
		}
	} else if isDFU, state := isDFURecoveryByName(sp.Name); isDFU {
		dev = &Device{
			Model:       sp.Name,
			State:       state,
			IsDFU:       true,
			USBLocation: sp.LocationID,
		}
		if ecid := extractECIDFromString(sp.SerialNum); ecid != "" {
			dev.ECID = ecid
		} else {
			debugLog("⚠️  DFU без ECID (%s) – игнор.", sp.Name)
			dev = nil
		}
	} else if isNormalMacDevice(sp) {
		dev = &Device{
			Model:       sp.Name,
			State:       "Normal",
			IsDFU:       false,
			USBLocation: sp.LocationID,
		}
	}

	// добавляем, если валидно
	if dev != nil && dev.UniqueID() != "" {
		*acc = append(*acc, dev)
		// подробный вывод теперь только в debug-режиме
		debugLog("🔍 Обнаружено: %s, UID=%s", dev.GetFriendlyName(), dev.UniqueID())
	}

	for i := range sp.SubItems {
		m.extractDevicesRecursively(&sp.SubItems[i], acc)
	}
}

/*
──────────────────────────────────────────────────────────

	Utils
	──────────────────────────────────────────────────────────
*/
func (m *Monitor) sendEvent(e Event) {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()

	select {
	case m.eventChan <- e:
	case <-m.ctx.Done():
		log.Println("ℹ️  Context canceled – событие пропущено.")
	case <-timer.C:
		log.Printf("⚠️  Буфер событий переполнен, %s для %s пропущено.",
			e.Type, e.Device.GetFriendlyName())
	}
}

func (m *Monitor) initialScan() error {
	log.Println("🔍 Первое сканирование USB…")
	devs := m.fetchCurrentUSBDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	dfu, normal := 0, 0
	for _, d := range devs {
		m.devices[d.UniqueID()] = d
		if d.IsDFU {
			dfu++
		} else {
			normal++
		}
	}
	log.Printf("✅ Найдено: %d DFU + %d Normal", dfu, normal)
	return nil
}

func (m *Monitor) GetConnectedDevices() []*Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	out := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		c := *d
		out = append(out, &c)
	}
	return out
}

/*
──────────────────────────────────────────────────────────

	NameResolver – связываем ECID ↔︎ Model
	──────────────────────────────────────────────────────────
*/
type NameResolver struct {
	ecid2name map[string]string // ECID -> Model
	usb2name  map[string]string // USB  -> Model (временно, пока нет ECID)
	mu        sync.RWMutex
}

func NewNameResolver() *NameResolver {
	return &NameResolver{
		ecid2name: make(map[string]string),
		usb2name:  make(map[string]string),
	}
}

func (r *NameResolver) rememberNormal(dev *Device) {
	if dev.USBLocation == "" || !dev.IsNormalMac() {
		return
	}
	r.mu.Lock()
	r.usb2name[dev.USBLocation] = dev.Model
	r.mu.Unlock()
}

// возвращает true, если удалось присвоить «человеческую» модель
func (r *NameResolver) tryBeautifyDFU(ctx context.Context, dev *Device) bool {
	if !dev.IsDFU || dev.ECID == "" {
		return false
	}

	// 1) уже знаем ECID → имя
	r.mu.RLock()
	if name, ok := r.ecid2name[dev.ECID]; ok {
		r.mu.RUnlock()
		dev.Model = name
		return true
	}
	// 2) знаем USB → имя
	if name, ok := r.usb2name[dev.USBLocation]; ok {
		r.mu.RUnlock()
		dev.Model = name
		r.mu.Lock()
		r.ecid2name[dev.ECID] = name // кэшируем навсегда
		r.mu.Unlock()
		return true
	}
	r.mu.RUnlock()

	// 3) спрашиваем cfgutil
	if name, _ := lookupModelWithCfgutil(ctx, dev.ECID); name != "" {
		dev.Model = name
		r.mu.Lock()
		r.ecid2name[dev.ECID] = name
		r.mu.Unlock()
		return true
	}
	return false
}

/*
cfgutil --format JSON -v list → модель
*/
type cfgutilList struct {
	Output map[string]struct {
		Name       *string `json:"name"`
		DeviceType string  `json:"deviceType"`
	} `json:"Output"`
}

func lookupModelWithCfgutil(ctx context.Context, ecid string) (string, error) {
	out, err := exec.CommandContext(ctx, "cfgutil", "--format", "JSON", "-v", "list").Output()
	if err != nil {
		return "", err
	}
	var data cfgutilList
	if err := json.Unmarshal(out, &data); err != nil {
		return "", err
	}
	key1 := strings.ToLower(ecid)
	key2 := "0x" + strings.ToUpper(strings.TrimPrefix(ecid, "0x"))
	for k, v := range data.Output {
		if strings.EqualFold(k, key1) || strings.EqualFold(k, key2) {
			if v.Name != nil && *v.Name != "" {
				return *v.Name, nil
			}
			return v.DeviceType, nil
		}
	}
	return "", fmt.Errorf("нет записи для ECID %s", ecid)
}
