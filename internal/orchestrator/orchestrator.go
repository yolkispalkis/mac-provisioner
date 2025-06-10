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
		knownDevices:    make(map[string]*model.Device),
		cooldowns:       make(map[string]time.Time),
		processing:      make(map[string]bool),
		processingPorts: make(map[string]bool),
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
					deviceScanChan <- devices
				} else {
					log.Printf("⚠️ Ошибка сканирования USB: %v", err)
				}
			}
		}
	}()

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

// processDeviceList анализирует список устройств и реагирует на изменения.
func (o *Orchestrator) processDeviceList(devices []*model.Device, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	currentDevices := make(map[string]bool)

	for _, dev := range devices {
		devID := dev.ID()
		if devID == "" { // Игнорируем устройства без ID
			continue
		}
		currentDevices[devID] = true

		if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
			continue
		}

		prev, exists := o.knownDevices[devID]

		// --- НАЧАЛО ГЛАВНОГО ИСПРАВЛЕНИЯ ---
		if exists && prev.Name != "" && dev.Name != prev.Name {
			// Если мы уже знаем хорошее имя для этого устройства (по ID),
			// а новое имя другое (например, "Apple Mobile Device..."),
			// то сохраняем старое, хорошее имя.
			dev.Name = prev.Name
		}
		// --- КОНЕЦ ГЛАВНОГО ИСПРАВЛЕНИЯ ---

		if !exists {
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
		} else if prev.State != dev.State {
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
		}

		// Обновляем запись в любом случае
		o.knownDevices[devID] = dev

		if dev.State == model.StateDFU && dev.ECID != "" {
			if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; onCooldown && time.Now().Before(cooldownTime) {
				// В кулдауне
			} else {
				o.processing[dev.ECID] = true
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

	delete(o.processing, result.Device.ECID)
	if result.Device.USBLocation != "" {
		delete(o.processingPorts, result.Device.USBLocation)
	}

	if result.Err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", result.Device.GetDisplayName(), result.Err)
		o.notifier.Speak("Ошибка прошивки " + result.Device.GetReadableName())
	} else {
		log.Printf("✅ Прошивка завершена для %s. Установлен кулдаун.", result.Device.GetDisplayName())
		o.notifier.Speak("Прошивка " + result.Device.GetDisplayName() + " завершена")
		o.cooldowns[result.Device.ECID] = time.Now().Add(o.cfg.DFUCooldown)
	}
}

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
