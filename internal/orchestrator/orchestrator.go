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

// processDeviceList - ПЕРЕПИСАННАЯ ВЕРСИЯ
func (o *Orchestrator) processDeviceList(devices []*model.Device, jobs chan<- *model.Device) {
	o.mu.Lock()
	defer o.mu.Unlock()

	currentDeviceIDs := make(map[string]bool)
	newDeviceMap := make(map[string]*model.Device)

	// 1. Создаем карту новых устройств и сохраняем "хорошие" имена из старого списка.
	for _, dev := range devices {
		devID := dev.ID()
		if devID == "" {
			continue
		}
		currentDeviceIDs[devID] = true

		// Если для этого ID у нас уже есть запись с хорошим именем, используем его.
		if oldDev, exists := o.knownDevices[devID]; exists && oldDev.Name != "" {
			if dev.Name != oldDev.Name {
				dev.Name = oldDev.Name // Принудительно ставим старое, хорошее имя.
			}
		}
		newDeviceMap[devID] = dev
	}

	// 2. Определяем события (подключено/изменилось)
	for devID, dev := range newDeviceMap {
		oldDev, exists := o.knownDevices[devID]

		// Игнорируем, если устройство уже в обработке
		if (dev.ECID != "" && o.processing[dev.ECID]) || (dev.USBLocation != "" && o.processingPorts[dev.USBLocation]) {
			continue
		}

		if !exists {
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
		} else if oldDev.State != dev.State {
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
		}

		// Проверяем, готово ли устройство к прошивке
		if dev.State == model.StateDFU && dev.ECID != "" {
			if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; !onCooldown || time.Now().After(cooldownTime) {
				o.processing[dev.ECID] = true
				if dev.USBLocation != "" {
					o.processingPorts[dev.USBLocation] = true
				}
				// Отправляем на прошивку устройство с уже исправленным именем
				jobs <- dev
			}
		}
	}

	// 3. Определяем отключенные устройства
	for devID, dev := range o.knownDevices {
		if !currentDeviceIDs[devID] {
			log.Printf("🔌 Отключено: %s", dev.GetDisplayName())
			o.notifier.Speak("Отключено " + dev.GetReadableName())
		}
	}

	// 4. Обновляем основную карту устройств
	o.knownDevices = newDeviceMap
}

func (o *Orchestrator) processProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Используем имя из результата, так как оно было зафиксировано при старте задачи
	displayName := result.Device.GetDisplayName()

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
