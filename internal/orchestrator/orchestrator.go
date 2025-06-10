package orchestrator

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/model"
	"mac-provisioner/internal/notifier"
)

type Orchestrator struct {
	cfg             *config.Config
	notifier        notifier.Notifier
	resolver        *Resolver // <-- Возвращаем резолвер
	knownDevices    map[string]*model.Device
	cooldowns       map[string]time.Time
	processing      map[string]bool
	processingPorts map[string]bool
	mu              sync.RWMutex
}

func New(cfg *config.Config, notifier notifier.Notifier) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		notifier:        notifier,
		resolver:        NewResolver(), // <-- Инициализируем резолвер
		knownDevices:    make(map[string]*model.Device),
		cooldowns:       make(map[string]time.Time),
		processing:      make(map[string]bool),
		processingPorts: make(map[string]bool),
	}
}

func (o *Orchestrator) Start(ctx context.Context) {
	log.Println("Orchestrator starting...")
	o.notifier.Speak("Система запущена")

	deviceScanChan := make(chan []*model.Device, 1)
	provisionJobsChan := make(chan *model.Device, o.cfg.MaxConcurrentJobs)
	provisionResultsChan := make(chan ProvisionResult, o.cfg.MaxConcurrentJobs)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(o.cfg.CheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 1. Получаем список от system_profiler
				devices, err := scanUSB(ctx)
				if err != nil {
					log.Printf("⚠️ Ошибка сканирования USB: %v", err)
					continue
				}

				// 2. Получаем список от cfgutil
				resolvedDevices := o.resolver.GetResolvedDevices(ctx)

				// 3. "Склеиваем" информацию
				o.mergeDeviceData(devices, resolvedDevices)

				// 4. Отправляем обогащенный список дальше
				deviceScanChan <- devices
			}
		}
	}()

	// ... остальная часть Start остается без изменений
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
		case devices := <-deviceScanChan:
			o.processDeviceList(devices, provisionJobsChan)
		case result := <-provisionResultsChan:
			o.processProvisionResult(result)
		case <-dfuTriggerTicker.C:
			o.checkAndTriggerDFU(ctx)
		}
	}
}

// mergeDeviceData обогащает устройства из system_profiler данными из cfgutil.
func (o *Orchestrator) mergeDeviceData(profilerDevices []*model.Device, cfgutilDevices map[string]*ResolvedDevice) {
	if len(cfgutilDevices) == 0 {
		return
	}

	for _, pDev := range profilerDevices {
		// Если у устройства уже есть ECID, ничего не делаем, доверяем сканеру
		if pDev.ECID != "" {
			continue
		}

		// Если ECID нет, ищем по LocationID
		if pDev.USBLocation != "" {
			baseLocation := strings.TrimSpace(strings.Split(pDev.USBLocation, "/")[0])
			if rDev, ok := cfgutilDevices[baseLocation]; ok {
				pDev.ECID = rDev.ECID
				pDev.Name = rDev.Name
			}
		}
	}
}

func (o *Orchestrator) processDeviceList(devices []*model.Device, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	currentDeviceIDs := make(map[string]bool)
	newDeviceMap := make(map[string]*model.Device)

	for _, dev := range devices {
		if dev.ECID != "" {
			dev.ECID = strings.ToLower(dev.ECID)
		}

		devID := dev.ID()
		if devID == "" {
			continue
		}
		currentDeviceIDs[devID] = true

		if oldDev, exists := o.knownDevices[devID]; exists && oldDev.Name != "" && !strings.Contains(strings.ToLower(dev.Name), "dfu") {
			dev.Name = oldDev.Name
		}
		newDeviceMap[devID] = dev
	}

	for devID, dev := range newDeviceMap {
		oldDev, exists := o.knownDevices[devID]

		if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
			continue
		}

		if !exists {
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
		} else if oldDev.State != dev.State || oldDev.Name != dev.Name {
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
		}

		if dev.State == model.StateDFU && dev.ECID != "" {
			if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; !onCooldown || time.Now().After(cooldownTime) {
				o.processing[dev.ECID] = true
				if dev.USBLocation != "" {
					o.processingPorts[dev.USBLocation] = true
				}
				jobs <- dev
			}
		}
	}

	for devID, dev := range o.knownDevices {
		if !currentDeviceIDs[devID] {
			log.Printf("🔌 Отключено: %s", dev.GetDisplayName())
		}
	}

	o.knownDevices = newDeviceMap
}

// ... Функции processProvisionResult и checkAndTriggerDFU остаются без изменений ...
func (o *Orchestrator) processProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	displayName := result.Device.GetDisplayName()
	ecidKey := strings.ToLower(result.Device.ECID)

	delete(o.processing, ecidKey)
	if result.Device.USBLocation != "" {
		delete(o.processingPorts, result.Device.USBLocation)
	}

	if result.Err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", displayName, result.Err)
		o.notifier.Speak("Ошибка прошивки " + displayName)
	} else {
		log.Printf("✅ Прошивка завершена для %s. Установлен кулдаун.", displayName)
		o.notifier.Speak("Прошивка " + displayName + " завершена")
		o.cooldowns[ecidKey] = time.Now().Add(o.cfg.DFUCooldown)
	}
}

func (o *Orchestrator) checkAndTriggerDFU(ctx context.Context) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	for _, dev := range o.knownDevices {
		if isDFUPort(dev.USBLocation) && dev.State == model.StateNormal {
			ecidKey := strings.ToLower(dev.ECID)
			if (ecidKey != "" && o.processing[ecidKey]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
				continue
			}

			if ecidKey != "" {
				if cooldownTime, onCooldown := o.cooldowns[ecidKey]; onCooldown && time.Now().Before(cooldownTime) {
					continue
				}
			}

			go triggerDFU(ctx)
			return
		}
	}
}
