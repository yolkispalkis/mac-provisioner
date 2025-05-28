package main

import (
	"context"
	"fmt"
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

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	notifier := notification.New(cfg.Notifications)
	statsMgr := stats.New()
	dfuManager := dfu.New()
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provisionerManager := provisioner.New(dfuManager, notifier, statsMgr)

	log.Println("🔧 Компоненты инициализированы")
	notifier.SystemStarted()

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Ошибка запуска мониторинга устройств: %v", err)
	}

	go handleDeviceEvents(ctx, deviceMonitor, provisionerManager, notifier, dfuManager)
	go printStats(ctx, statsMgr, 10*time.Minute)
	go debugConnectedDevices(ctx, deviceMonitor, 30*time.Second)

	log.Println("✅ Mac Provisioner запущен. Нажмите Ctrl+C для остановки.")
	log.Println("🔌 Подключите Mac через USB-C для автоматической прошивки...")

	<-sigChan
	log.Println("🛑 Завершение работы Mac Provisioner...")
	notifier.SystemShutdown()

	cancel()
	deviceMonitor.Stop()

	log.Println("⏳ Ожидание завершения фоновых задач...")
	time.Sleep(3 * time.Second)
	log.Println("👋 Mac Provisioner остановлен.")
}

func handleDeviceEvents(ctx context.Context, monitor *device.Monitor, provManager *provisioner.Manager, notifier *notification.Manager, dfuManager *dfu.Manager) {
	log.Println("🎧 Запуск обработчика событий устройств...")

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
				if event.Device.IsDFU && event.Device.ECID != "" {
					// Устройство уже в DFU/Recovery - сразу прошиваем
					log.Printf("🔧 DFU устройство %s (ECID: %s) готово к прошивке.",
						event.Device.GetFriendlyName(), event.Device.ECID)
					notifier.DeviceDetected(event.Device)
					go provManager.ProcessDevice(ctx, event.Device)
				} else if event.Device.IsNormalMac() {
					// Обычный Mac - переводим в DFU
					log.Printf("💻 Обнаружен обычный Mac %s (%s). Переводим в DFU режим...",
						event.Device.GetFriendlyName(), event.Device.SerialNumber)
					notifier.DeviceConnected(event.Device)
					notifier.EnteringDFUMode(event.Device)

					go func(dev *device.Device) {
						if err := dfuManager.EnterDFUMode(ctx, dev.SerialNumber); err != nil {
							log.Printf("❌ Не удалось перевести %s в DFU: %v", dev.SerialNumber, err)
							if err.Error() == "macvdmtool недоступен, автоматический вход в DFU невозможен" {
								notifier.ManualDFURequired(dev)
								dfuManager.OfferManualDFU(dev.SerialNumber)
							} else {
								notifier.Error(fmt.Sprintf("Ошибка входа в DFU для %s: %v", dev.SerialNumber, err))
							}
						} else {
							log.Printf("✅ Устройство %s успешно переведено в DFU режим", dev.SerialNumber)
							notifier.DFUModeEntered(dev)
						}
					}(event.Device)
				} else {
					log.Printf("❓ Подключено неизвестное устройство: %s (DFU: %v, ECID: '%s')",
						event.Device.GetFriendlyName(), event.Device.IsDFU, event.Device.ECID)
				}

			case device.EventDisconnected:
				log.Printf("🔌 Отключено устройство: %s (%s)",
					event.Device.SerialNumber, event.Device.GetFriendlyName())
				notifier.DeviceDisconnected(event.Device)

			case device.EventStateChanged:
				log.Printf("🔄 Изменение состояния устройства: %s (%s) - %s. DFU: %v",
					event.Device.SerialNumber, event.Device.GetFriendlyName(), event.Device.State, event.Device.IsDFU)

				// Если устройство перешло в DFU и готово к прошивке
				if event.Device.IsDFU && event.Device.ECID != "" {
					log.Printf("🔧 Устройство %s перешло в DFU и готово к прошивке.",
						event.Device.GetFriendlyName())
					notifier.DFUModeEntered(event.Device)
					go provManager.ProcessDevice(ctx, event.Device)
				} else if event.Device.IsNormalMac() {
					// Устройство вернулось в обычный режим (возможно, после прошивки)
					log.Printf("✅ Устройство %s вернулось в обычный режим.",
						event.Device.GetFriendlyName())
					notifier.DeviceReady(event.Device)
				}
			}
		}
	}
}

func printStats(ctx context.Context, statsMgr *stats.Manager, interval time.Duration) {
	if interval == 0 {
		interval = 10 * time.Minute
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
		interval = 30 * time.Second
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
			devicesList := monitor.GetConnectedDevices()
			if len(devicesList) > 0 {
				dfuCount := 0
				normalCount := 0
				for _, dev := range devicesList {
					if dev.IsDFU {
						dfuCount++
					} else {
						normalCount++
					}
				}
				log.Printf("🔍 Отладка: Подключено устройств: %d DFU/Recovery + %d обычных Mac", dfuCount, normalCount)
				for i, dev := range devicesList {
					if dev.IsDFU {
						log.Printf("  %d. DFU: %s (Модель: %s, Состояние: %s, ECID: %s, NeedsProv: %v)",
							i+1, dev.SerialNumber, dev.Model, dev.State, dev.ECID, dev.NeedsProvisioning())
					} else {
						log.Printf("  %d. MAC: %s (Модель: %s, Состояние: %s, NeedsProv: %v)",
							i+1, dev.SerialNumber, dev.Model, dev.State, dev.NeedsProvisioning())
					}
				}
			} else {
				log.Println("🔍 Отладка: Подключенных Apple устройств не обнаружено.")
			}
		}
	}
}
