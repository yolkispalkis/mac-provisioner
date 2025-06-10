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

// Orchestrator управляет всем жизненным циклом приложения.
type Orchestrator struct {
	cfg             *config.Config
	notifier        notifier.Notifier
	resolver        *Resolver
	knownDevices    map[string]*model.Device
	cooldowns       map[string]time.Time
	processing      map[string]bool
	processingPorts map[string]bool
	resolvedNames   map[string]string // Долгоживущий кэш точных имен [ECID -> Name]
	mu              sync.RWMutex
}

func New(cfg *config.Config, notifier notifier.Notifier) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		notifier:        notifier,
		resolver:        NewResolver(),
		knownDevices:    make(map[string]*model.Device),
		cooldowns:       make(map[string]time.Time),
		processing:      make(map[string]bool),
		processingPorts: make(map[string]bool),
		resolvedNames:   make(map[string]string),
	}
}

// Start запускает все рабочие процессы и главный цикл оркестратора.
func (o *Orchestrator) Start(ctx context.Context) {
	log.Println("Orchestrator starting...")
	o.notifier.Speak("Система запущена")

	deviceScanChan := make(chan []*model.Device, 1)
	provisionJobsChan := make(chan *model.Device, o.cfg.MaxConcurrentJobs)
	provisionResultsChan := make(chan ProvisionResult, o.cfg.MaxConcurrentJobs)

	var wg sync.WaitGroup

	// Воркер 1: Сканер устройств
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
				// 1. Получаем сырые данные от сканера
				devices, err := scanUSB(ctx)
				if err != nil {
					log.Printf("⚠️ Ошибка сканирования USB: %v", err)
					continue
				}

				// 2. Получаем точные имена от резолвера
				resolved := o.resolver.GetResolvedNames(ctx)

				o.mu.Lock()
				// 3. Обновляем глобальный кэш имен
				for ecid, name := range resolved {
					if name != "" {
						o.resolvedNames[ecid] = name
					}
				}

				// 4. Принудительно применяем имена из кэша ко всем устройствам
				for _, dev := range devices {
					if dev.ECID != "" {
						// Приводим ECID к единому формату для ключа
						ecidKey := strings.ToLower(dev.ECID)
						if name, ok := o.resolvedNames[ecidKey]; ok {
							dev.Name = name
						}
					}
				}
				o.mu.Unlock()

				// 5. Отправляем обогащенные данные в оркестратор
				deviceScanChan <- devices
			}
		}
	}()

	// Остальная часть Start остается без изменений...
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

func (o *Orchestrator) processDeviceList(devices []*model.Device, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	currentDevices := make(map[string]bool)

	for _, dev := range devices {
		devID := dev.ID()
		if devID == "" {
			continue
		}
		currentDevices[devID] = true

		if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
			continue
		}

		prev, exists := o.knownDevices[devID]
		if !exists {
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
		} else if prev.State != dev.State || prev.Name != dev.Name {
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
		}

		o.knownDevices[devID] = dev

		if dev.State == model.StateDFU && dev.ECID != "" {
			ecidKey := strings.ToLower(dev.ECID)
			if cooldownTime, onCooldown := o.cooldowns[ecidKey]; !onCooldown || time.Now().After(cooldownTime) {
				o.processing[ecidKey] = true
				if dev.USBLocation != "" {
					o.processingPorts[dev.USBLocation] = true
				}
				jobs <- dev
			}
		}
	}

	for id, dev := range o.knownDevices {
		if !currentDevices[id] {
			log.Printf("🔌 Отключено: %s", dev.GetDisplayName())
			o.notifier.Speak("Отключено " + dev.GetReadableName())
			delete(o.knownDevices, id)
		}
	}
}

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
