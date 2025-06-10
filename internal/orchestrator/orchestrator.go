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
	cfg          *config.Config
	notifier     notifier.Notifier
	knownDevices map[string]*model.Device // Карта известных устройств [ID -> Device]
	cooldowns    map[string]time.Time     // Карта кулдаунов [ECID -> CooldownUntil]
	processing   map[string]bool          // Карта устройств в процессе прошивки [ECID -> true]
	mu           sync.RWMutex             // Мьютекс для защиты карт
}

func New(cfg *config.Config, notifier notifier.Notifier) *Orchestrator {
	return &Orchestrator{
		cfg:          cfg,
		notifier:     notifier,
		knownDevices: make(map[string]*model.Device),
		cooldowns:    make(map[string]time.Time),
		processing:   make(map[string]bool),
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
				// Запускаем каждую прошивку в своей горутине, чтобы не блокировать пул
				go runProvisioning(ctx, job, provisionResultsChan)
			}
		}
	}()

	// Главный цикл Оркестратора
	dfuTriggerTicker := time.NewTicker(o.cfg.CheckInterval * 2) // Проверяем реже, чем сканируем
	defer dfuTriggerTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Orchestrator shutting down...")
			wg.Wait() // Дожидаемся завершения всех воркеров
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
		currentDevices[devID] = true

		if dev.ECID != "" && o.processing[dev.ECID] {
			continue // Устройство уже в процессе, пропускаем
		}

		prev, exists := o.knownDevices[devID]
		if !exists {
			// --- УЛУЧШЕННЫЙ ЛОГ ---
			log.Printf("🔌 Подключено: %s (State: %s, ECID: %s)", dev.GetDisplayName(), dev.State, dev.ECID)
			o.notifier.Speak("Подключено " + dev.GetReadableName())
			o.knownDevices[devID] = dev
		} else if prev.State != dev.State {
			// --- УЛУЧШЕННЫЙ ЛОГ ---
			log.Printf("🔄 Изменение состояния: %s -> %s (ECID: %s)", prev.GetDisplayName(), dev.State, dev.ECID)
			o.knownDevices[devID] = dev
		}

		// Проверяем, готово ли устройство к прошивке
		if dev.State == model.StateDFU && dev.ECID != "" {
			if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; onCooldown && time.Now().Before(cooldownTime) {
				// Находится в кулдауне, ничего не делаем
			} else {
				// Отправляем на прошивку
				o.processing[dev.ECID] = true
				jobs <- dev
			}
		}
	}

	// Ищем отключенные устройства
	for id, dev := range o.knownDevices {
		if !currentDevices[id] {
			log.Printf("🔌 Отключено: %s", dev.GetDisplayName())
			o.notifier.Speak("Отключено " + dev.GetReadableName())
			delete(o.knownDevices, id)
		}
	}
}

// processProvisionResult обрабатывает результат завершенной прошивки.
func (o *Orchestrator) processProvisionResult(result ProvisionResult) {
	o.mu.Lock()
	defer o.mu.Unlock()

	delete(o.processing, result.Device.ECID)

	if result.Err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", result.Device.GetDisplayName(), result.Err)
		o.notifier.Speak("Ошибка прошивки " + result.Device.GetReadableName())
	} else {
		log.Printf("✅ Прошивка завершена для %s. Установлен кулдаун.", result.Device.GetDisplayName())
		o.notifier.Speak("Прошивка " + result.Device.GetReadableName() + " завершена")
		o.cooldowns[result.Device.ECID] = time.Now().Add(o.cfg.DFUCooldown)
	}
}

// checkAndTriggerDFU проверяет, нужно ли запустить macvdmtool.
func (o *Orchestrator) checkAndTriggerDFU(ctx context.Context) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	// Ищем устройство в нормальном режиме на DFU-порту, которое не в кулдауне и не в обработке
	for _, dev := range o.knownDevices {
		if isDFUPort(dev.USBLocation) && dev.State == model.StateNormal {
			if dev.ECID != "" {
				if o.processing[dev.ECID] {
					continue
				}
				if cooldownTime, onCooldown := o.cooldowns[dev.ECID]; onCooldown && time.Now().Before(cooldownTime) {
					continue
				}
			}
			// Нашли кандидата, запускаем DFU и выходим
			go triggerDFU(ctx)
			return
		}
	}
}
