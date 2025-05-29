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

/*
──────────────────────────────────────────────────────────

	Debug-флаг для периодического списка устройств

──────────────────────────────────────────────────────────
*/
var showDeviceList = os.Getenv("MAC_PROV_DEBUG") == "1"

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

	// Core
	notifier := notification.New(cfg.Notifications)
	statsMgr := stats.New()
	dfuMgr := dfu.New()
	devMon := device.NewMonitor(cfg.Monitoring)
	provMgr := provisioner.New(dfuMgr, notifier, statsMgr)

	notifier.SystemStarted()

	if err := devMon.Start(ctx); err != nil {
		log.Fatalf("❌ Не удалось запустить монитор устройств: %v", err)
	}

	go handleDeviceEvents(ctx, devMon, provMgr, notifier, dfuMgr)

	// Периодический вывод устройств только при MAC_PROV_DEBUG=1
	if showDeviceList {
		go debugConnectedDevices(ctx, devMon, 30*time.Second)
	}

	log.Println("✅ Mac Provisioner запущен. Нажмите Ctrl+C для выхода.")
	log.Println("🔌 Подключите Mac через USB-C для автоматической прошивки...")

	<-sigChan

	// graceful-shutdown
	log.Println("🛑 Завершение работы...")
	notifier.SystemShutdown()
	cancel()
	devMon.Stop()
	time.Sleep(2 * time.Second)
	log.Println("👋 Готово.")
}

/*
──────────────────────────────────────────────────────────

	Device events

──────────────────────────────────────────────────────────
*/
func handleDeviceEvents(
	ctx context.Context,
	mon *device.Monitor,
	prov *provisioner.Manager,
	notif *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-mon.Events():
			if !ok {
				return
			}

			// подавляем события от порта, где уже идёт прошивка
			if prov.IsProcessingUSB(ev.Device.USBLocation) {
				continue
			}

			log.Printf("📨 %s: %s", strings.ToUpper(ev.Type), ev.Device.GetFriendlyName())

			switch ev.Type {
			case device.EventConnected:
				onConnected(ctx, ev.Device, prov, notif, dfuMgr)
			case device.EventDisconnected:
				notif.DeviceDisconnected(ev.Device)
			case device.EventStateChanged:
				onStateChanged(ctx, ev.Device, prov, notif, dfuMgr)
			}
		}
	}
}

func onConnected(
	ctx context.Context,
	dev *device.Device,
	prov *provisioner.Manager,
	notif *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	// === DFU-устройство ===
	if dev.IsDFU && dev.ECID != "" {
		// DFU-mode – готово к прошивке
		if strings.EqualFold(dev.State, "DFU") {
			notif.DeviceDetected(dev)
			go prov.ProcessDevice(ctx, dev)
			return
		}

		// Recovery-mode – нужно перевести в DFU
		if strings.EqualFold(dev.State, "Recovery") {
			if prov.IsProcessingUSB(dev.USBLocation) {
				return
			}
			notif.DeviceDetected(dev)
			enterDFUFlow(ctx, dev, notif, dfuMgr)
			return
		}
	}

	// === Живой Mac (Normal) ===
	if dev.IsNormalMac() {
		if prov.IsProcessingUSB(dev.USBLocation) {
			return
		}
		enterDFUFlow(ctx, dev, notif, dfuMgr)
	}
}

func onStateChanged(
	ctx context.Context,
	dev *device.Device,
	prov *provisioner.Manager,
	notif *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	// Перешёл в DFU → можно шить
	if dev.IsDFU && dev.ECID != "" && strings.EqualFold(dev.State, "DFU") {
		notif.DFUModeEntered(dev)
		go prov.ProcessDevice(ctx, dev)
		return
	}

	// Перешёл в Recovery → снова просим DFU
	if dev.IsDFU && dev.ECID != "" && strings.EqualFold(dev.State, "Recovery") {
		enterDFUFlow(ctx, dev, notif, dfuMgr)
		return
	}

	// Вернулся в Normal
	if dev.IsNormalMac() {
		notif.DeviceReady(dev)
	}
}

/*
enterDFUFlow – общая функция для Normal-Mac и Recovery:
пытаемся автоматом через macvdmtool, иначе просим вручную.
*/
func enterDFUFlow(
	ctx context.Context,
	dev *device.Device,
	notif *notification.Manager,
	dfuMgr *dfu.Manager,
) {
	notif.DeviceConnected(dev)
	notif.EnteringDFUMode(dev)

	go func(d *device.Device) {
		if err := dfuMgr.EnterDFUMode(ctx, d.USBLocation); err != nil {
			if err.Error() == "macvdmtool недоступен, автоматический вход в DFU невозможен" {
				notif.ManualDFURequired(d)
				dfuMgr.OfferManualDFU(d.USBLocation)
			} else {
				notif.Error(fmt.Sprintf("Ошибка входа в DFU: %v", err))
			}
		} else {
			notif.DFUModeEntered(d)
		}
	}(dev)
}

/*
──────────────────────────────────────────────────────────

	Debug device list

──────────────────────────────────────────────────────────
*/
func debugConnectedDevices(
	ctx context.Context,
	mon *device.Monitor,
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
			list := mon.GetConnectedDevices()
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
