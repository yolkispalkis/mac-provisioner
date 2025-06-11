package orchestrator

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
	"mac-provisioner/internal/model"
	"mac-provisioner/internal/notifier"
)

// --- КОД ИЗ SCANNER.GO, ВКЛЮЧАЯ ВСЕ ВСПОМОГАТЕЛЬНЫЕ ФУНКЦИИ ---

// EventType определяет тип события, связанного с устройством.
type EventType string

const (
	EventConnected    EventType = "Connected"
	EventDisconnected EventType = "Disconnected"
)

// DeviceEvent представляет событие, связанное с устройством.
type DeviceEvent struct {
	Type   EventType
	Device *model.Device
}

// Scanner отслеживает USB-устройства и генерирует события.
type Scanner struct {
	interval     time.Duration
	knownDevices map[string]*model.Device // Ключ: USB Location
	mu           sync.Mutex
}

func NewScanner(interval time.Duration) *Scanner {
	return &Scanner{
		interval:     interval,
		knownDevices: make(map[string]*model.Device),
	}
}

// Start запускает цикл сканирования, отправляя события в канал.
func (s *Scanner) Start(ctx context.Context, eventChan chan<- DeviceEvent) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	// Выполняем первое сканирование сразу
	s.scanAndEmitEvents(ctx, eventChan)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scanAndEmitEvents(ctx, eventChan)
		}
	}
}

func (s *Scanner) scanAndEmitEvents(ctx context.Context, eventChan chan<- DeviceEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	devices, err := scanUSB(ctx) // <--- ВОТ ВЫЗОВ
	if err != nil {
		log.Printf("⚠️ Ошибка сканирования USB: %v", err)
		return
	}

	currentDevices := make(map[string]bool)
	for _, dev := range devices {
		if dev.USBLocation == "" {
			continue
		}
		currentDevices[dev.USBLocation] = true

		oldDev, exists := s.knownDevices[dev.USBLocation]
		if !exists || oldDev.State != dev.State || oldDev.ECID != dev.ECID {
			s.knownDevices[dev.USBLocation] = dev
			eventChan <- DeviceEvent{Type: EventConnected, Device: dev}
		}
	}

	for location, dev := range s.knownDevices {
		if !currentDevices[location] {
			delete(s.knownDevices, location)
			eventChan <- DeviceEvent{Type: EventDisconnected, Device: dev}
		}
	}
}

// --- НЕДОСТАЮЩИЕ ФУНКЦИИ ---

// scanUSB выполняет системный вызов для получения списка USB-устройств.
func scanUSB(ctx context.Context) ([]*model.Device, error) {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var data struct {
		USB []json.RawMessage `json:"SPUSBDataType"`
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return nil, err
	}

	var devices []*model.Device
	for _, rawItem := range data.USB {
		devices = append(devices, parseDeviceTree(rawItem)...)
	}

	return devices, nil
}

// parseDeviceTree рекурсивно обходит дерево USB-устройств из JSON.
func parseDeviceTree(rawItem json.RawMessage) []*model.Device {
	var item struct {
		Name         string            `json:"_name"`
		ProductID    string            `json:"product_id"`
		VendorID     string            `json:"vendor_id"`
		SerialNum    string            `json:"serial_num"`
		LocationID   string            `json:"location_id"`
		Manufacturer string            `json:"manufacturer"`
		Items        []json.RawMessage `json:"_items"`
	}
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return nil
	}

	var found []*model.Device

	if strings.EqualFold(item.VendorID, "0x05ac") || strings.Contains(item.Manufacturer, "Apple") {
		if dev := createDeviceFromProfiler(&item); dev != nil {
			found = append(found, dev)
		}
	}

	for _, subItem := range item.Items {
		found = append(found, parseDeviceTree(subItem)...)
	}

	return found
}

func isValidHexECID(s string) bool {
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(s) < 10 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func createDeviceFromProfiler(item *struct {
	Name         string            `json:"_name"`
	ProductID    string            `json:"product_id"`
	VendorID     string            `json:"vendor_id"`
	SerialNum    string            `json:"serial_num"`
	LocationID   string            `json:"location_id"`
	Manufacturer string            `json:"manufacturer"`
	Items        []json.RawMessage `json:"_items"`
}) *model.Device {
	dev := &model.Device{
		Name:        item.Name,
		USBLocation: item.LocationID,
		State:       model.StateUnknown,
	}

	name := strings.ToLower(item.Name)
	isDFUProduct := item.ProductID == "0x1281" || item.ProductID == "0x1227"
	isRecoveryProduct := item.ProductID == "0x1280"

	if strings.Contains(name, "dfu mode") || isDFUProduct {
		dev.State = model.StateDFU
	} else if strings.Contains(name, "recovery mode") || isRecoveryProduct {
		dev.State = model.StateRecovery
	} else if item.SerialNum != "" && len(item.SerialNum) > 5 {
		dev.State = model.StateNormal
	} else {
		return nil
	}

	if parts := strings.Split(item.SerialNum, "-"); len(parts) == 2 && isValidHexECID(parts[1]) {
		ecidStr := strings.ToLower(parts[1])
		if !strings.HasPrefix(ecidStr, "0x") {
			dev.ECID = "0x" + ecidStr
		} else {
			dev.ECID = ecidStr
		}
		return dev
	}

	re := regexp.MustCompile(`(?i)ECID:?\s*([0-9A-F]+)`)
	matches := re.FindStringSubmatch(item.SerialNum)
	if len(matches) > 1 {
		dev.ECID = "0x" + strings.ToLower(matches[1])
		return dev
	}

	if isValidHexECID(item.SerialNum) {
		ecidStr := strings.ToLower(item.SerialNum)
		if !strings.HasPrefix(ecidStr, "0x") {
			dev.ECID = "0x" + ecidStr
		} else {
			dev.ECID = ecidStr
		}
		return dev
	}

	return dev
}

// --- КОД ОРКЕСТРАТОРА ---

// DeviceState представляет полное состояние известного устройства.
type DeviceState struct {
	*model.Device
	AccurateName string // Самое точное имя, которое мы знаем
}

type Orchestrator struct {
	cfg      *config.Config
	notifier notifier.Notifier
	resolver *Resolver

	devicesByPort   map[string]*DeviceState
	devicesByECID   map[string]*DeviceState
	cooldowns       map[string]time.Time
	processingPorts map[string]bool

	mu sync.RWMutex
}

func New(cfg *config.Config, notifier notifier.Notifier) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		notifier:        notifier,
		resolver:        NewResolver(),
		devicesByPort:   make(map[string]*DeviceState),
		devicesByECID:   make(map[string]*DeviceState),
		cooldowns:       make(map[string]time.Time),
		processingPorts: make(map[string]bool),
	}
}

func (o *Orchestrator) Start(ctx context.Context) {
	log.Println("Orchestrator starting...")
	o.notifier.Speak("Система запущена")

	eventChan := make(chan DeviceEvent, 10)
	provisionJobsChan := make(chan *model.Device, o.cfg.MaxConcurrentJobs)
	provisionResultsChan := make(chan ProvisionResult, o.cfg.MaxConcurrentJobs)

	var wg sync.WaitGroup

	scanner := NewScanner(o.cfg.CheckInterval)
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner.Start(ctx, eventChan)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-provisionJobsChan:
				go runProvisioning(ctx, job, provisionResultsChan)
			}
		}
	}()

	dfuTriggerTicker := time.NewTicker(o.cfg.CheckInterval * 2)
	defer dfuTriggerTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Orchestrator shutting down...")
			wg.Wait()
			return
		case event := <-eventChan:
			o.handleDeviceEvent(ctx, event, provisionJobsChan)
		case result := <-provisionResultsChan:
			o.handleProvisionResult(result)
		case <-dfuTriggerTicker.C:
			o.checkAndTriggerDFU(ctx)
		}
	}
}

func (o *Orchestrator) handleDeviceEvent(ctx context.Context, event DeviceEvent, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	switch event.Type {
	case EventConnected:
		o.onDeviceConnected(ctx, event.Device, jobs)
	case EventDisconnected:
		o.onDeviceDisconnected(event.Device)
	}
}

func (o *Orchestrator) onDeviceConnected(ctx context.Context, dev *model.Device, jobs chan<- *model.Device) {
	if dev.USBLocation != "" && o.processingPorts[dev.USBLocation] {
		return
	}

	if dev.ECID == "" {
		resolved, err := o.resolver.GetInfoByLocation(ctx)
		if err == nil {
			baseLocation := strings.Split(dev.USBLocation, "/")[0]
			if info, ok := resolved[baseLocation]; ok {
				dev.ECID = info.ECID
				dev.Name = info.Name
			}
		}
	}

	if dev.ECID != "" {
		dev.ECID = strings.ToLower(dev.ECID)
	}

	state := &DeviceState{Device: dev}
	if dev.ECID != "" {
		if existing, ok := o.devicesByECID[dev.ECID]; ok && existing.AccurateName != "" {
			state.AccurateName = existing.AccurateName
		}
	}

	if dev.State == model.StateNormal && dev.Name != "" {
		state.AccurateName = dev.Name
	}

	log.Printf("🔌 Подключено/Обновлено: %s (State: %s, ECID: %s)", state.Device.GetDisplayName(), state.Device.State, state.Device.ECID)

	if dev.USBLocation != "" {
		o.devicesByPort[dev.USBLocation] = state
	}
	if dev.ECID != "" {
		o.devicesByECID[dev.ECID] = state
	}

	if dev.State == model.StateDFU && dev.ECID != "" {
		if cooldown, ok := o.cooldowns[dev.ECID]; ok && time.Now().Before(cooldown) {
			log.Printf("🕒 Устройство %s в кулдауне", state.AccurateName)
			return
		}

		if dev.USBLocation != "" {
			o.processingPorts[dev.USBLocation] = true
		}

		jobDev := *dev
		if state.AccurateName != "" {
			jobDev.Name = state.AccurateName
		}

		jobs <- &jobDev
	}
}

func (o *Orchestrator) onDeviceDisconnected(dev *model.Device) {
	if dev.USBLocation == "" {
		return
	}

	state, exists := o.devicesByPort[dev.USBLocation]
	if !exists {
		return
	}

	delete(o.devicesByPort, dev.USBLocation)

	var displayName = dev.GetDisplayName()
	if state.AccurateName != "" {
		displayName = state.AccurateName
	}

	log.Printf("🔌 Отключено: %s", displayName)
}

func (o *Orchestrator) handleProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	ecid := strings.ToLower(result.Device.ECID)

	var displayName = result.Device.GetDisplayName()
	if state, ok := o.devicesByECID[ecid]; ok && state.AccurateName != "" {
		displayName = state.AccurateName
	}

	if state, ok := o.devicesByECID[ecid]; ok {
		delete(o.processingPorts, state.USBLocation)
	}

	if result.Err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", displayName, result.Err)
		o.notifier.Speak("Ошибка прошивки " + displayName)
	} else {
		log.Printf("✅ Прошивка завершена для %s. Установлен кулдаун.", displayName)
		o.notifier.Speak("Прошивка " + displayName + " завершена")
		o.cooldowns[ecid] = time.Now().Add(o.cfg.DFUCooldown)
	}
}

func (o *Orchestrator) checkAndTriggerDFU(ctx context.Context) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	for port, state := range o.devicesByPort {
		if o.processingPorts[port] {
			continue
		}

		if isDFUPort(port) && state.State == model.StateNormal {
			if state.ECID != "" {
				if cooldown, ok := o.cooldowns[state.ECID]; ok && time.Now().Before(cooldown) {
					continue
				}
			}
			var name = state.AccurateName
			if name == "" {
				name = state.GetDisplayName()
			}

			log.Printf("⚡️ Запуск DFU для %s на порту %s", name, port)
			go triggerDFU(ctx)
			return
		}
	}
}
