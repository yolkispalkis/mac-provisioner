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
		// Устройство в Recovery режиме - переводим в DFU
		notifier.DeviceConnected(dev)
		notifier.EnteringDFUMode(dev)
		go enterDFUMode(ctx, dev, dfuMgr, notifier, provisioner)
	} else if dev.IsNormalMac() {
		// Обычный Mac - переводим в DFU
		notifier.DeviceConnected(dev)
		notifier.EnteringDFUMode(dev)
		go enterDFUMode(ctx, dev, dfuMgr, notifier, provisioner)
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

func enterDFUMode(
	ctx context.Context,
	dev *device.Device,
	dfuMgr *dfu.Manager,
	notifier *notification.Manager,
	provisioner *provisioner.Manager,
) {
	// Помечаем USB порт как обрабатываемый
	provisioner.MarkUSBProcessing(dev.USBLocation, true)
	defer provisioner.MarkUSBProcessing(dev.USBLocation, false)

	if err := dfuMgr.EnterDFUMode(ctx, dev.USBLocation); err != nil {
		notifier.ManualDFURequired(dev)
	}
}
