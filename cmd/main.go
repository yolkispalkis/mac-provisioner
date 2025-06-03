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
	provisionerManager := provisioner.New(dfuManager, notifier)

	// Контекст для graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Обработка сигналов завершения
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Запуск системы
	notifier.SystemStarted()

	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("❌ Не удалось запустить мониторинг устройств: %v", err)
	}

	// Обработка событий устройств
	go handleDeviceEvents(ctx, deviceMonitor, provisionerManager, notifier, dfuManager)

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

	switch event.Type {
	case device.EventConnected:
		if dev.IsDFU && dev.State == "DFU" {
			notifier.DFUModeEntered(dev)
			go provisioner.ProcessDevice(ctx, dev)
		} else if dev.IsDFU && dev.State == "Recovery" {
			notifier.DeviceConnected(dev)
			notifier.EnteringDFUMode(dev)
			go enterDFUMode(ctx, dev, dfuMgr, notifier)
		} else if dev.IsNormalMac() {
			notifier.DeviceConnected(dev)
			notifier.EnteringDFUMode(dev)
			go enterDFUMode(ctx, dev, dfuMgr, notifier)
		}

	case device.EventDisconnected:
		notifier.DeviceDisconnected(dev)

	case device.EventStateChanged:
		if dev.IsDFU && dev.State == "DFU" {
			notifier.DFUModeEntered(dev)
			go provisioner.ProcessDevice(ctx, dev)
		}
	}
}

func enterDFUMode(ctx context.Context, dev *device.Device, dfuMgr *dfu.Manager, notifier *notification.Manager) {
	if err := dfuMgr.EnterDFUMode(ctx, dev.USBLocation); err != nil {
		notifier.ManualDFURequired(dev)
	}
}
