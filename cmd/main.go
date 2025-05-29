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
	log.Printf("⚙️  Интервал проверки устройств: %v  |  Голос уведомлений: %s",
		cfg.Monitoring.CheckInterval, cfg.Notifications.Voice)

	ctx, cancel := context.WithCancel(context.Background())

	// Ctrl-C / kill
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Core components
	notifier := notification.New(cfg.Notifications)
	statsMgr := stats.New()
	dfuManager := dfu.New()
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provManager := provisioner.New(dfuManager, notifier, statsMgr)

	notifier.SystemStarted()

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Не удалось запустить монитор устройств: %v", err)
	}

	go handleDeviceEvents(ctx, deviceMonitor, provManager, notifier, dfuManager)
	go debugConnectedDevices(ctx, deviceMonitor, 30*time.Second)

	log.Println("✅ Mac Provisioner запущен. Нажмите Ctrl+C для выхода.")
	log.Println("🔌 Подключите Mac через USB-C для автоматической прошивки...")

	<-sigChan

	// graceful-shutdown
	log.Println("🛑 Завершение работы...")
	notifier.SystemShutdown()
	cancel()
	deviceMonitor.Stop()
	time.Sleep(2 * time.Second)
	log.Println("👋 Готово.")
}

/*
──────────────────────────────────────────────────────────

	Обработчик событий устройств

──────────────────────────────────────────────────────────
*/
func handleDeviceEvents(
	ctx context.Context,
	monitor *device.Monitor,
	provManager *provisioner.Manager,
	notifier *notification.Manager,
	dfuManager *dfu.Manager,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-monitor.Events():
			if !ok {
				return
			}
			log.Printf("📨 %s: %s", strings.ToUpper(ev.Type), ev.Device.GetFriendlyName())

			switch ev.Type {
			case device.EventConnected:
				onConnected(ctx, ev.Device, provManager, notifier, dfuManager)
			case device.EventDisconnected:
				notifier.DeviceDisconnected(ev.Device)
			case device.EventStateChanged:
				onStateChanged(ctx, ev.Device, provManager, notifier)
			}
		}
	}
}

func onConnected(
	ctx context.Context,
	dev *device.Device,
	prov *provisioner.Manager,
	notifier *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	// Если это уже DFU-устройство с ECID — сразу на прошивку.
	if dev.IsDFU && dev.ECID != "" {
		notifier.DeviceDetected(dev)
		go prov.ProcessDevice(ctx, dev)
		return
	}

	// Обычный Mac (Normal mode) — захотим перевести в DFU.
	if dev.IsNormalMac() {

		// NEW: если этот USB-порт уже «занят» активной прошивкой —
		// ничего не делаем, чтобы не сбросить устройство обратно в DFU.
		if prov.IsProcessingUSB(dev.USBLocation) {
			log.Printf("ℹ️ %s уже прошивается (USB %s) — авто-DFU пропущен.",
				dev.GetFriendlyName(), strings.TrimPrefix(dev.USBLocation, "0x"))
			return
		}

		notifier.DeviceConnected(dev)
		notifier.EnteringDFUMode(dev)

		// Пытаемся через macvdmtool
		go func(d *device.Device) {
			if err := dfuMgr.EnterDFUMode(ctx, d.USBLocation); err != nil {
				if err.Error() == "macvdmtool недоступен, автоматический вход в DFU невозможен" {
					notifier.ManualDFURequired(d)
					dfuMgr.OfferManualDFU(d.USBLocation)
				} else {
					notifier.Error(fmt.Sprintf("Ошибка входа в DFU: %v", err))
				}
			} else {
				notifier.DFUModeEntered(d)
			}
		}(dev)
	}
}

func onStateChanged(
	ctx context.Context,
	dev *device.Device,
	prov *provisioner.Manager,
	notifier *notification.Manager,
) {
	if dev.IsDFU && dev.ECID != "" {
		notifier.DFUModeEntered(dev)
		go prov.ProcessDevice(ctx, dev)
	} else if dev.IsNormalMac() {
		notifier.DeviceReady(dev)
	}
}

/*
──────────────────────────────────────────────────────────

	Периодический список подключённых устройств (debug)

──────────────────────────────────────────────────────────
*/
func debugConnectedDevices(
	ctx context.Context,
	monitor *device.Monitor,
	interval time.Duration,
) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			list := monitor.GetConnectedDevices()
			if len(list) == 0 {
				log.Println("🔍 Устройств не обнаружено.")
				continue
			}
			dfuCount, normalCount := 0, 0
			for _, d := range list {
				if d.IsDFU {
					dfuCount++
				} else {
					normalCount++
				}
			}
			log.Printf("🔍 Подключено: %d DFU + %d Normal", dfuCount, normalCount)

			for i, d := range list {
				if d.IsDFU {
					log.Printf("  %d. DFU: %s (State:%s)", i+1, d.GetFriendlyName(), d.State)
				} else {
					log.Printf("  %d. MAC: %s (USB:%s, State:%s)",
						i+1, d.GetFriendlyName(),
						strings.TrimPrefix(d.USBLocation, "0x"),
						d.State)
				}
			}
		}
	}
}
