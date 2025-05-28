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
	"mac-provisioner/internal/stats"
)

func main() {
	log.Println("🚀 Запуск Mac Provisioner...")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("❌ Ошибка загрузки конфигурации: %v", err)
	}
	log.Printf("⚙️ Конфигурация загружена: интервал проверки %v, голос %s",
		cfg.Monitoring.CheckInterval, cfg.Notifications.Voice)

	ctx, cancel := context.WithCancel(context.Background())
	// defer cancel() // Отменяем в конце main

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	notifier := notification.New(cfg.Notifications)
	statsMgr := stats.New() // Изменено имя переменной для избежания конфликта с пакетом
	dfuManager := dfu.New()
	// Передаем cfg.Monitoring в NewMonitor
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provisionerManager := provisioner.New(dfuManager, notifier, statsMgr)

	log.Println("🔧 Компоненты инициализированы")
	notifier.SystemStarted()

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Ошибка запуска мониторинга устройств: %v", err)
	}
	// defer deviceMonitor.Stop() // Stop вызывается перед выходом из main

	go handleDeviceEvents(ctx, deviceMonitor, provisionerManager, notifier)
	go printStats(ctx, statsMgr, 10*time.Minute) // Передаем statsMgr
	go debugConnectedDevices(ctx, deviceMonitor, 30*time.Second)

	log.Println("✅ Mac Provisioner запущен. Нажмите Ctrl+C для остановки.")
	log.Println("🔌 Подключите Mac в DFU/Recovery режиме через USB-C для автоматической прошивки...")

	<-sigChan // Ожидаем сигнал завершения
	log.Println("🛑 Завершение работы Mac Provisioner...")
	notifier.SystemShutdown() // Уведомление о завершении

	cancel() // Отменяем контекст, чтобы все горутины завершились

	deviceMonitor.Stop() // Останавливаем монитор явно

	// Даем время для завершения уведомлений и других фоновых задач
	log.Println("⏳ Ожидание завершения фоновых задач...")
	time.Sleep(3 * time.Second)
	log.Println("👋 Mac Provisioner остановлен.")
}

func handleDeviceEvents(ctx context.Context, monitor *device.Monitor, provManager *provisioner.Manager, notifier *notification.Manager) {
	log.Println("🎧 Запуск обработчика событий устройств (DFU/Recovery)...")

	for {
		select {
		case <-ctx.Done():
			log.Println("🛑 Обработчик событий устройств остановлен (контекст завершен).")
			return
		case event, ok := <-monitor.Events():
			if !ok {
				log.Println("🛑 Канал событий устройств закрыт, обработчик останавливается.")
				return
			}

			log.Printf("📨 Получено событие: %s для устройства %s (Модель: %s, ECID: %s, DFU: %v)",
				event.Type, event.Device.SerialNumber, event.Device.Model, event.Device.ECID, event.Device.IsDFU)

			switch event.Type {
			case device.EventConnected:
				// С новым монитором, мы в основном будем видеть только DFU/Recovery устройства.
				log.Printf("🔌 Подключено устройство: %s (%s) - состояние: %s, DFU: %v",
					event.Device.SerialNumber, event.Device.GetFriendlyName(), event.Device.State, event.Device.IsDFU)

				// NeedsProvisioning теперь просто проверяет IsDFU (и валидность ECID)
				if event.Device.NeedsProvisioning() {
					log.Printf("🔧 Устройство %s (ECID: %s) нуждается в прошивке.",
						event.Device.GetFriendlyName(), event.Device.ECID)
					notifier.DeviceDetected(event.Device) // Уведомление о DFU устройстве
					go provManager.ProcessDevice(ctx, event.Device)
				} else {
					// Этот блок маловероятен, если NeedsProvisioning = IsDFU
					log.Printf("❓ Устройство %s (%s) подключено, но не требует прошивки (IsDFU: %v, ECID: '%s'). Это неожиданно.",
						event.Device.GetFriendlyName(), event.Device.State, event.Device.IsDFU, event.Device.ECID)
					// notifier.DeviceReady(event.Device) // DeviceReady для DFU устройств не имеет смысла
				}

			case device.EventDisconnected:
				log.Printf("🔌 Отключено устройство: %s (%s)",
					event.Device.SerialNumber, event.Device.GetFriendlyName())
				notifier.DeviceDisconnected(event.Device)

			case device.EventStateChanged:
				// Изменение состояния для DFU устройств маловероятно, кроме как при отключении.
				// Если оно перешло из DFU в не-DFU, оно исчезнет из system_profiler (и будет Disconnected).
				log.Printf("🔄 Изменение состояния устройства: %s (%s) - %s. DFU: %v",
					event.Device.SerialNumber, event.Device.GetFriendlyName(), event.Device.State, event.Device.IsDFU)

				// Если оно как-то изменилось, но все еще DFU и требует прошивки
				if event.Device.NeedsProvisioning() {
					log.Printf("🔧 Устройство %s (%s) изменило состояние, но все еще нуждается в прошивке.",
						event.Device.GetFriendlyName(), event.Device.State)
					go provManager.ProcessDevice(ctx, event.Device)
				}
			}
		}
	}
}

// printStats и debugConnectedDevices с небольшими изменениями для имен переменных и интервалов
func printStats(ctx context.Context, statsMgr *stats.Manager, interval time.Duration) {
	if interval == 0 {
		interval = 10 * time.Minute // Значение по умолчанию
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("📊 Статистика будет выводиться каждые %v", interval)
	for {
		select {
		case <-ctx.Done():
			log.Println("📊 Вывод статистики остановлен.")
			return
		case <-ticker.C:
			log.Printf("📊 Статистика: %s", statsMgr.Summary())
		}
	}
}

func debugConnectedDevices(ctx context.Context, monitor *device.Monitor, interval time.Duration) {
	if interval == 0 {
		interval = 30 * time.Second // Значение по умолчанию
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("🔍 Отладка подключенных устройств будет выводиться каждые %v", interval)
	for {
		select {
		case <-ctx.Done():
			log.Println("🔍 Отладка подключенных устройств остановлена.")
			return
		case <-ticker.C:
			devicesList := monitor.GetConnectedDevices() // Имя переменной изменено
			if len(devicesList) > 0 {
				log.Printf("🔍 Отладка: Подключено DFU/Recovery устройств: %d", len(devicesList))
				for i, dev := range devicesList {
					log.Printf("  %d. SN: %s (Модель: %s, Состояние: %s, ECID: %s, DFU: %v, NeedsProv: %v)",
						i+1, dev.SerialNumber, dev.Model, dev.State, dev.ECID, dev.IsDFU, dev.NeedsProvisioning())
				}
			} else {
				log.Println("🔍 Отладка: Подключенных DFU/Recovery устройств не обнаружено (system_profiler).")
			}
		}
	}
}
