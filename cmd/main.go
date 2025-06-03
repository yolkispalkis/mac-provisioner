package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
	"mac-provisioner/internal/provisioner"
	"mac-provisioner/internal/voice"
)

func main() {
	log.Println("🚀 Запуск Mac Provisioner...")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("❌ Ошибка загрузки конфигурации: %v", err)
	}

	// Инициализация компонентов
	voiceEngine := voice.New(cfg.Notifications)
	defer voiceEngine.Shutdown()

	notifier := notification.New(cfg.Notifications, voiceEngine)
	dfuManager := dfu.New()
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provisionerManager := provisioner.New(dfuManager, notifier, cfg.Provisioning)

	// Настраиваем автоматический DFU триггер и проверки
	deviceMonitor.SetDFUTrigger(dfuManager.AutoTriggerDFU)
	deviceMonitor.SetProcessingChecker(func(usbLocation string) bool {
		return provisionerManager.IsProcessingByUSB(usbLocation)
	})
	deviceMonitor.SetCooldownChecker(func(usbLocation string) (bool, time.Duration, string) {
		return provisionerManager.IsInCooldown(usbLocation)
	})

	// Контекст для graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Обработка сигналов завершения
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Запуск системы
	notifier.SystemStarted()
	log.Printf("⚙️ Период охлаждения DFU: %v", cfg.Provisioning.DFUCooldownPeriod)

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Не удалось запустить мониторинг устройств: %v", err)
	}

	// Обработка событий устройств
	go handleDeviceEvents(ctx, deviceMonitor, provisionerManager, notifier, dfuManager)

	// Периодический вывод статуса периодов охлаждения (для отладки)
	if os.Getenv("MAC_PROV_DEBUG") == "1" {
		go debugCooldownStatus(ctx, provisionerManager, 5*time.Minute)
	}

	// Ожидание сигнала завершения
	<-sigChan

	log.Println("🛑 Завершение работы...")
	notifier.SystemShutdown()
	cancel()
	deviceMonitor.Stop()
	time.Sleep(time.Second)
	log.Println("✅ Завершено")
}

func handleDeviceEvents(
	ctx context.Context,
	monitor *device.Monitor,
	provisioner *provisioner.Manager,
	notifier *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-monitor.Events():
			handleSingleEvent(ctx, event, provisioner, notifier, dfuMgr)
		}
	}
}

func handleSingleEvent(
	ctx context.Context,
	event device.Event,
	provisioner *provisioner.Manager,
	notifier *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	dev := event.Device
	log.Printf("📨 %s: %s", event.Type, dev.Name)

	// Игнорируем события для устройств, которые уже обрабатываются
	if provisioner.IsProcessingByUSB(dev.USBLocation) || provisioner.IsProcessingByECID(dev.ECID) {
		log.Printf("ℹ️ Устройство уже обрабатывается, игнорируем событие: %s", dev.Name)
		return
	}

	switch event.Type {
	case device.EventConnected:
		handleDeviceConnected(ctx, dev, provisioner, notifier, dfuMgr)

	case device.EventDisconnected:
		// Уведомляем об отключении только если устройство не обрабатывается
		if !provisioner.IsProcessingByUSB(dev.USBLocation) && !provisioner.IsProcessingByECID(dev.ECID) {
			notifier.DeviceDisconnected(dev)
		}

	case device.EventStateChanged:
		handleDeviceStateChanged(ctx, dev, provisioner, notifier, dfuMgr)
	}
}

func handleDeviceConnected(
	ctx context.Context,
	dev *device.Device,
	provisioner *provisioner.Manager,
	notifier *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	if dev.IsDFU && dev.State == "DFU" && dev.ECID != "" {
		// Устройство уже в DFU режиме - начинаем прошивку
		notifier.DFUModeEntered(dev)
		go provisioner.ProcessDevice(ctx, dev)
	} else if dev.IsDFU && dev.State == "Recovery" {
		// Устройство в Recovery режиме - проверяем период охлаждения
		if inCooldown, remaining, lastDevice := provisioner.IsInCooldown(dev.USBLocation); inCooldown {
			log.Printf("🕒 Устройство %s подключено к порту в периоде охлаждения (осталось %v, последнее: %s)",
				dev.Name, remaining.Round(time.Minute), lastDevice)
			notifier.DeviceConnected(dev)
		} else {
			notifier.DeviceConnected(dev)
			notifier.EnteringDFUMode(dev)
		}
	} else if dev.IsNormalMac() {
		// Обычный Mac - проверяем период охлаждения
		if inCooldown, remaining, lastDevice := provisioner.IsInCooldown(dev.USBLocation); inCooldown {
			log.Printf("🕒 Устройство %s подключено к порту в периоде охлаждения (осталось %v, последнее: %s)",
				dev.Name, remaining.Round(time.Minute), lastDevice)
			notifier.DeviceConnected(dev)
		} else {
			notifier.DeviceConnected(dev)
			notifier.EnteringDFUMode(dev)
		}
	}
}

func handleDeviceStateChanged(
	ctx context.Context,
	dev *device.Device,
	provisioner *provisioner.Manager,
	notifier *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	// Обрабатываем только переход в DFU режим
	if dev.IsDFU && dev.State == "DFU" && dev.ECID != "" {
		notifier.DFUModeEntered(dev)
		go provisioner.ProcessDevice(ctx, dev)
	}
}

// debugCooldownStatus периодически выводит информацию о периодах охлаждения
func debugCooldownStatus(ctx context.Context, provisioner *provisioner.Manager, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cooldowns := provisioner.GetCooldownStatus()
			if len(cooldowns) == 0 {
				log.Println("🕒 Активных периодов охлаждения нет")
			} else {
				log.Printf("🕒 Активные периоды охлаждения (%d):", len(cooldowns))
				for i, entry := range cooldowns {
					remaining := time.Until(entry.CooldownUntil)
					log.Printf("  %d. %s (порт: %s, осталось: %v)",
						i+1, entry.DeviceName, entry.USBLocation, remaining.Round(time.Minute))
				}
			}
		}
	}
}
