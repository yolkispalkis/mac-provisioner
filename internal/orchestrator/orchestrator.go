package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/model"
	"mac-provisioner/internal/notifier"
)

func normalizeECID(rawECID string) (string, error) {
	cleanECID := strings.TrimPrefix(strings.ToLower(rawECID), "0x")
	val, err := strconv.ParseUint(cleanECID, 16, 64)
	if err != nil {
		return "", fmt.Errorf("не удалось спарсить ECID '%s': %w", rawECID, err)
	}
	return fmt.Sprintf("0x%x", val), nil
}

type EventType string

const (
	EventConnected    EventType = "Connected"
	EventDisconnected EventType = "Disconnected"
)

type DeviceEvent struct {
	Type   EventType
	Device *model.Device
}
type Scanner struct {
	interval     time.Duration
	knownDevices map[string]*model.Device
	mu           sync.Mutex
	infoLogger   *log.Logger
	debugLogger  *log.Logger
}

func NewScanner(interval time.Duration, infoLogger, debugLogger *log.Logger) *Scanner {
	return &Scanner{
		interval:     interval,
		knownDevices: make(map[string]*model.Device),
		infoLogger:   infoLogger,
		debugLogger:  debugLogger,
	}
}
func (s *Scanner) Start(ctx context.Context, eventChan chan<- DeviceEvent) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
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
	devices, err := scanUSB(ctx)
	if err != nil {
		s.infoLogger.Printf("[WARN] Ошибка сканирования USB: %v", err)
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
	if len(s) < 10 || len(s) > 24 {
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
	dev := &model.Device{Name: item.Name, USBLocation: item.LocationID, State: model.StateUnknown}
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
	var rawECID string
	if parts := strings.Split(item.SerialNum, "-"); len(parts) == 2 && isValidHexECID(parts[1]) {
		rawECID = parts[1]
	} else if matches := regexp.MustCompile(`(?i)ECID:?\s*([0-9A-F]+)`).FindStringSubmatch(item.SerialNum); len(matches) > 1 {
		rawECID = matches[1]
	} else if isValidHexECID(item.SerialNum) {
		rawECID = item.SerialNum
	}
	if rawECID != "" {
		normalized, err := normalizeECID(rawECID)
		if err == nil {
			dev.ECID = normalized
		} else {
			log.Printf("[WARN] Не удалось нормализовать ECID от system_profiler: %s", rawECID)
		}
	}
	return dev
}

type DeviceState struct {
	*model.Device
	AccurateName string
}

type Orchestrator struct {
	cfg             *config.Config
	notifier        notifier.Notifier
	resolver        *Resolver
	devicesByPort   map[string]*DeviceState
	devicesByECID   map[string]*DeviceState
	cooldowns       map[string]time.Time
	processingPorts map[string]bool
	mu              sync.RWMutex
	activeJobs      int
	infoLogger      *log.Logger
	debugLogger     *log.Logger
}

func New(cfg *config.Config, notifier notifier.Notifier, infoLogger, debugLogger *log.Logger) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		notifier:        notifier,
		resolver:        NewResolver(infoLogger, debugLogger),
		devicesByPort:   make(map[string]*DeviceState),
		devicesByECID:   make(map[string]*DeviceState),
		cooldowns:       make(map[string]time.Time),
		processingPorts: make(map[string]bool),
		activeJobs:      0,
		infoLogger:      infoLogger,
		debugLogger:     debugLogger,
	}
}

func (o *Orchestrator) Start(ctx context.Context) {
	o.infoLogger.Println("Orchestrator starting...")
	o.notifier.Speak("Система запущена")
	eventChan := make(chan DeviceEvent, 10)
	provisionJobsChan := make(chan *model.Device, o.cfg.MaxConcurrentJobs)
	provisionResultsChan := make(chan ProvisionResult, o.cfg.MaxConcurrentJobs)
	provisionUpdateChan := make(chan ProvisionUpdate, o.cfg.MaxConcurrentJobs)
	var wg sync.WaitGroup
	scanner := NewScanner(o.cfg.CheckInterval, o.infoLogger, o.debugLogger)
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
				go runProvisioning(ctx, job, provisionResultsChan, provisionUpdateChan, o.infoLogger)
			}
		}
	}()
	dfuTriggerTicker := time.NewTicker(o.cfg.CheckInterval * 2)
	defer dfuTriggerTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			o.infoLogger.Println("Orchestrator shutting down...")
			wg.Wait()
			return
		case event := <-eventChan:
			o.handleDeviceEvent(ctx, event, provisionJobsChan)
		case result := <-provisionResultsChan:
			o.handleProvisionResult(result)
		case update := <-provisionUpdateChan:
			o.handleProvisionUpdate(update)
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
		o.debugLogger.Printf("Порт %s уже в обработке, пропускаем.", dev.USBLocation)
		return
	}
	state := &DeviceState{Device: dev}
	if dev.ECID != "" {
		if existing, ok := o.devicesByECID[dev.ECID]; ok && existing.AccurateName != "" {
			state.AccurateName = existing.AccurateName
			state.Device.Name = existing.AccurateName
		}
	}
	if dev.ECID != "" {
		resolved, err := o.resolver.GetInfoByECID(ctx)
		if err == nil {
			if info, ok := resolved[dev.ECID]; ok {
				o.debugLogger.Printf("Найдена точная информация для ECID %s: Name=%s", dev.ECID, info.Name)
				if info.Name != "" {
					state.Device.Name = info.Name
					state.AccurateName = info.Name
				}
			}
		} else {
			o.infoLogger.Printf("[WARN] Не удалось получить информацию от cfgutil: %v", err)
		}
	}
	o.infoLogger.Printf("Подключено/Обновлено: %s (Состояние: %s, ECID: %s)", state.Device.GetDisplayName(), state.Device.State, state.Device.ECID)
	if dev.USBLocation != "" {
		o.devicesByPort[dev.USBLocation] = state
	}
	if dev.ECID != "" {
		o.devicesByECID[dev.ECID] = state
	}
	if dev.State == model.StateDFU && dev.ECID != "" {
		if cooldown, ok := o.cooldowns[dev.ECID]; ok && time.Now().Before(cooldown) {
			o.infoLogger.Printf("Устройство %s (%s) в кулдауне, прошивка отложена.", state.Device.GetDisplayName(), dev.ECID)
			o.notifier.Speak("Подключено " + state.Device.GetReadableName() + ", но в кулдауне")
			return
		}
		if dev.USBLocation != "" {
			o.processingPorts[dev.USBLocation] = true
		}
		o.activeJobs++
		o.debugLogger.Printf("Новое задание, активных прошивок: %d", o.activeJobs)
		jobDev := *state.Device
		o.debugLogger.Printf("Отправляем %s на прошивку.", jobDev.GetDisplayName())
		o.notifier.SpeakImmediately("Начинаю прошивку " + jobDev.GetReadableName())
		jobs <- &jobDev
	} else {
		o.notifier.Speak("Подключено " + state.Device.GetReadableName())
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
	o.infoLogger.Printf("Отключено: %s", displayName)
}

func (o *Orchestrator) handleProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.activeJobs--
	if o.activeJobs < 0 {
		o.activeJobs = 0
	}
	o.debugLogger.Printf("Завершено задание, активных прошивок: %d", o.activeJobs)
	ecid := result.Device.ECID
	var displayName = result.Device.GetDisplayName()
	if state, ok := o.devicesByECID[ecid]; ok && state.AccurateName != "" {
		displayName = state.AccurateName
	}
	if state, ok := o.devicesByECID[ecid]; ok {
		delete(o.processingPorts, state.USBLocation)
	}
	if result.Err != nil {
		o.infoLogger.Printf("[ERROR] Ошибка прошивки %s: %v", displayName, result.Err)
		o.notifier.SpeakImmediately("Ошибка прошивки " + result.Device.GetReadableName())
	} else {
		o.infoLogger.Printf("Прошивка завершена для %s. Установлен кулдаун.", displayName)
		o.notifier.SpeakImmediately("Прошивка " + result.Device.GetReadableName() + " успешно завершена")
		o.cooldowns[ecid] = time.Now().Add(o.cfg.DFUCooldown)
	}
	if o.activeJobs == 0 {
		o.debugLogger.Printf("Все прошивки завершены, запускаем очистку кеша.")
		go cleanupConfiguratorCache(o.infoLogger, o.debugLogger)
	}
}

func (o *Orchestrator) handleProvisionUpdate(update ProvisionUpdate) {
	var displayName = update.Device.GetReadableName()
	o.mu.RLock()
	if state, ok := o.devicesByECID[update.Device.ECID]; ok && state.AccurateName != "" {
		displayName = state.AccurateName
	}
	o.mu.RUnlock()
	o.notifier.Announce(fmt.Sprintf("%s, этап %s", displayName, update.Status))
}

func (o *Orchestrator) checkAndTriggerDFU(ctx context.Context) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for port, state := range o.devicesByPort {
		if !isDFUPort(port) {
			continue
		}
		if o.processingPorts[port] {
			o.debugLogger.Printf("Порт для авто-DFU (%s) занят, пропуск.", port)
			continue
		}
		if state.State != model.StateNormal {
			continue
		}
		if state.ECID != "" {
			if cooldown, ok := o.cooldowns[state.ECID]; ok && time.Now().Before(cooldown) {
				o.debugLogger.Printf("Устройство %s на DFU-порту в кулдауне, пропуск.", state.GetDisplayName())
				continue
			}
		}
		var name = state.GetDisplayName()
		if state.AccurateName != "" {
			name = state.AccurateName
		}
		o.infoLogger.Printf("Обнаружено устройство %s на свободном DFU-порту. Запуск авто-DFU...", name)
		o.notifier.SpeakImmediately("Перевожу " + name + " в режим ДФУ")
		go triggerDFU(ctx, o.infoLogger)
		return
	}
}
