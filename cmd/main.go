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
	log.Println("Запуск Mac Provisioner...")

	// Загружаем конфигурацию
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Ошибка загрузки конфигурации: %v", err)
	}

	// Создаем контекст для graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Обработка сигналов завершения
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Инициализируем компоненты
	notifier := notification.New(cfg.Notifications)
	stats := stats.New()
	dfuManager := dfu.New()
	deviceMonitor := device.NewMonitor(cfg.Monitoring)
	provisionerManager := provisioner.New(dfuManager, notifier, stats)

	// Уведомление о запуске
	notifier.SystemStarted()

	// Запускаем мониторинг устройств
	if err := deviceMonitor.Start(ctx); err != nil {
		log.Fatalf("Ошибка запуска мониторинга устройств: %v", err)
	}
	defer deviceMonitor.Stop()

	// Обрабатываем события устройств
	go handleDeviceEvents(ctx, deviceMonitor, provisionerManager, notifier)

	// Периодический вывод статистики
	go printStats(ctx, stats)

	log.Println("Mac Provisioner запущен. Нажмите Ctrl+C для остановки.")

	// Ожидаем сигнал завершения
	<-sigChan
	log.Println("Завершение работы...")
	notifier.SystemShutdown()
	cancel()

	// Даем время для воспроизведения последнего сообщения
	time.Sleep(3 * time.Second)
}

func handleDeviceEvents(ctx context.Context, monitor *device.Monitor, provisioner *provisioner.Manager, notifier *notification.Manager) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-monitor.Events():
			switch event.Type {
			case device.EventConnected:
				log.Printf("Подключено устройство: %s (%s)", event.Device.SerialNumber, event.Device.Model)

				if event.Device.NeedsProvisioning() {
					notifier.DeviceDetected(event.Device.SerialNumber, event.Device.Model)
					go provisioner.ProcessDevice(ctx, event.Device)
				}

			case device.EventDisconnected:
				log.Printf("Отключено устройство: %s (%s)", event.Device.SerialNumber, event.Device.Model)
				notifier.DeviceDisconnected(event.Device.SerialNumber, event.Device.Model)

			case device.EventStateChanged:
				log.Printf("Изменение состояния устройства: %s (%s) - %s",
					event.Device.SerialNumber, event.Device.Model, event.Device.State)
			}
		}
	}
}

func printStats(ctx context.Context, stats *stats.Manager) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("Статистика: %s", stats.Summary())
		}
	}
}
