package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
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
	log.Printf("⚙️ Конфигурация: интервал проверки %v, голос %s",
		cfg.Monitoring.CheckInterval, cfg.Notifications.Voice)

	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	notifier := notification.New(cfg.Notifications)
	statsMgr := stats.New()
	dfuManager := dfu.New()
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provManager := provisioner.New(dfuManager, notifier, statsMgr)

	log.Println("🔧 Компоненты инициализированы")
	notifier.SystemStarted()

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Ошибка запуска мониторинга устройств: %v", err)
	}

	go handleDeviceEvents(ctx, deviceMonitor, provManager, notifier, dfuManager)
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

// ──────────────────────────────────────────────────────────
// Обработчик событий устройств
// ──────────────────────────────────────────────────────────
func handleDeviceEvents(ctx context.Context, monitor *device.Monitor, provManager *provisioner.Manager,
	notifier *notification.Manager, dfuManager *dfu.Manager) {

	log.Println("🎧 Запуск обработчика событий устройств...")

	for {
		select {
		case <-ctx.Done():
			log.Println("🛑 Обработчик событий остановлен (контекст завершён).")
			return

		case event, ok := <-monitor.Events():
			if !ok {
				log.Println("🛑 Канал событий закрыт, обработчик останавливается.")
				return
			}

			log.Printf("📨 %s: %s", strings.ToUpper(event.Type), event.Device.GetFriendlyName())

			switch event.Type {
			case device.EventConnected:
				handleConnected(ctx, event.Device, provManager, notifier, dfuManager)

			case device.EventDisconnected:
				log.Printf("🔌 Отключено устройство: %s", event.Device.GetFriendlyName())
				notifier.DeviceDisconnected(event.Device)

			case device.EventStateChanged:
				handleStateChanged(ctx, event.Device, provManager, notifier)
			}
		}
	}
}

func handleConnected(ctx context.Context, dev *device.Device, prov *provisioner.Manager,
	notifier *notification.Manager, dfuMgr *dfu.Manager) {

	if dev.IsDFU && dev.ECID != "" {
		// Уже в DFU — прошиваем.
		log.Printf("🔧 DFU устройство %s готово к прошивке.", dev.GetFriendlyName())
		notifier.DeviceDetected(dev)
		go prov.ProcessDevice(ctx, dev)

	} else if dev.IsNormalMac() {
		// Normal Mac — переводим в DFU.
		log.Printf("💻 Обнаружен обычный Mac %s. Перевод в DFU...", dev.GetFriendlyName())
		notifier.DeviceConnected(dev)
		notifier.EnteringDFUMode(dev)

		go func(d *device.Device) {
			if err := dfuMgr.EnterDFUMode(ctx, d.USBLocation); err != nil {
				log.Printf("❌ Не удалось перевести %s в DFU: %v", d.GetFriendlyName(), err)
				if err.Error() == "macvdmtool недоступен, автоматический вход в DFU невозможен" {
					notifier.ManualDFURequired(d)
					dfuMgr.OfferManualDFU(d.USBLocation)
				} else {
					notifier.Error(fmt.Sprintf("Ошибка входа в DFU: %v", err))
				}
			} else {
				log.Printf("✅ Устройство %s переведено в DFU", d.GetFriendlyName())
				notifier.DFUModeEntered(d)
			}
		}(dev)
	} else {
		log.Printf("❓ Неизвестное устройство: %s (DFU: %v, ECID: '%s')",
			dev.GetFriendlyName(), dev.IsDFU, dev.ECID)
	}
}

func handleStateChanged(ctx context.Context, dev *device.Device,
	prov *provisioner.Manager, notifier *notification.Manager) {

	log.Printf("🔄 Состояние изменилось: %s – %s, DFU:%v", dev.GetFriendlyName(), dev.State, dev.IsDFU)

	if dev.IsDFU && dev.ECID != "" {
		log.Printf("🔧 %s перешло в DFU и готово к прошивке.", dev.GetFriendlyName())
		notifier.DFUModeEntered(dev)
		go prov.ProcessDevice(ctx, dev)

	} else if dev.IsNormalMac() {
		log.Printf("✅ %s вернулось в нормальный режим.", dev.GetFriendlyName())
		notifier.DeviceReady(dev)
	}
}

// ──────────────────────────────────────────────────────────

func printStats(ctx context.Context, statsMgr *stats.Manager, interval time.Duration) {
	if interval == 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("📊 Статистика каждые %v", interval)
	for {
		select {
		case <-ctx.Done():
			log.Println("📊 Вывод статистики остановлен.")
			return
		case <-ticker.C:
			log.Printf("📊 %s", statsMgr.Summary())
		}
	}
}

func debugConnectedDevices(ctx context.Context, monitor *device.Monitor, interval time.Duration) {
	if interval == 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("🔍 Отладка списка устройств каждые %v", interval)
	for {
		select {
		case <-ctx.Done():
			log.Println("🔍 Отладка остановлена.")
			return
		case <-ticker.C:
			list := monitor.GetConnectedDevices()
			if len(list) == 0 {
				log.Println("🔍 Устройств не обнаружено.")
				continue
			}

			dfu, normal := 0, 0
			for _, d := range list {
				if d.IsDFU {
					dfu++
				} else {
					normal++
				}
			}
			log.Printf("🔍 Подключено: %d DFU + %d Normal", dfu, normal)
			for i, d := range list {
				if d.IsDFU {
					log.Printf("  %d. DFU: %s (ECID:%s, State:%s, NeedsProv:%v)",
						i+1, d.GetFriendlyName(), d.ECID, d.State, d.NeedsProvisioning())
				} else {
					log.Printf("  %d. MAC: %s (USB:%s, State:%s, NeedsProv:%v)",
						i+1, d.GetFriendlyName(), d.USBLocation, d.State, d.NeedsProvisioning())
				}
			}
		}
	}
}
