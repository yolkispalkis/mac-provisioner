package orchestrator

import (
	"context"
	"log"
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

	// --- НОВОЕ ПОЛЕ ---
	resolvedNames map[string]string // <-- Кэш точных имен [ECID -> Name]

	mu sync.RWMutex
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
		resolvedNames:   make(map[string]string), // <-- Инициализация
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
				if devices, err := scanUSB(ctx); err == nil {
					resolved := o.resolver.ResolveAndUpdate(ctx, devices)

					// --- ИЗМЕНЕНИЕ: Сохраняем распознанные имена ---
					o.mu.Lock()
					for ecid, info := range resolved {
						if info.Name != "" {
							o.resolvedNames[ecid] = info.Name
						}
					}
					o.mu.Unlock()

					deviceScanChan <- devices
				} else {
					log.Printf("⚠️ Ошибка сканирования USB: %v", err)
				}
			}
		}
	}()

	// Остальная часть Start остается без изменений...
	// Воркер 2: Пул прошивальщиков
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

	// Главный цикл Оркестратора
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

// getDisplayNameWithCache возвращает лучшее имя для устройства, используя кэш.
func (o *Orchestrator) getDisplayNameWithCache(dev *model.Device) string {
	if dev.ECID != "" {
		if name, ok := o.resolvedNames[dev.ECID]; ok {
			return name
		}
	}
	return dev.GetDisplayName()
}

// processDeviceList анализирует список устройств и реагирует на изменения.
func (o *Orchestrator) processDeviceList(devices []*model.Device, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	currentDevices := make(map[string]bool)

	for _, dev := range devices {
		devID := dev.ID()
		currentDevices[devID] = true

		// --- ИЗМЕНЕНИЕ: Используем имя из кэша ---
		displayName := o.getDisplayNameWithCache(dev)

		if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
			continue
		}

		prev, exists := o.knownDevices[devID]
		if !exists {
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", displayName, dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
			o.knownDevices[devID] = dev
		} else if prev.State != dev.State || prev.Name != dev.Name {
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", displayName, dev.State, dev.ECID)
			o.knownDevices[devID] = dev
		}

		if dev.State == model.StateDFU && dev.ECID != "" {
			if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; onCooldown && time.Now().Before(cooldownTime) {
				// В кулдауне
			} else {
				// Создаем копию устройства для задания, чтобы избежать гонки данных
				jobDev := *dev
				jobDev.Name = displayName // Передаем в задание точное имя

				o.processing[dev.ECID] = true
				if dev.USBLocation != "" {
					o.processingPorts[dev.USBLocation] = true
				}
				jobs <- &jobDev
			}
		}
	}

	for id, dev := range o.knownDevices {
		if !currentDevices[id] {
			displayName := o.getDisplayNameWithCache(dev)
			log.Printf("🔌 Отключено: %s", displayName)
			o.notifier.Speak("Отключено " + dev.GetReadableName())
			delete(o.knownDevices, id)
		}
	}
}

// processProvisionResult обрабатывает результат завершенной прошивки.
func (o *Orchestrator) processProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	displayName := o.getDisplayNameWithCache(result.Device)

	delete(o.processing, result.Device.ECID)
	if result.Device.USBLocation != "" {
		delete(o.processingPorts, result.Device.USBLocation)
	}

	if result.Err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", displayName, result.Err)
		o.notifier.Speak("Ошибка прошивки " + displayName)
	} else {
		log.Printf("✅ Прошивка завершена для %s. Установлен кулдаун.", displayName)
		o.notifier.Speak("Прошивка " + displayName + " завершена")
		o.cooldowns[result.Device.ECID] = time.Now().Add(o.cfg.DFUCooldown)
	}
}

// checkAndTriggerDFU проверяет, нужно ли запустить macvdmtool.
func (o *Orchestrator) checkAndTriggerDFU(ctx context.Context) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	for _, dev := range o.knownDevices {
		if isDFUPort(dev.USBLocation) && dev.State == model.StateNormal {
			if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
				continue
			}

			if dev.ECID != "" {
				if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; onCooldown && time.Now().Before(cooldownTime) {
					continue
				}
			}

			go triggerDFU(ctx)
			return
		}
	}
}
