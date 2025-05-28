package main

import (
	"context"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"

	"mac-provisioner/internal/configurator"
	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
	"mac-provisioner/internal/stats"
	"mac-provisioner/internal/usbmonitor"
)

type Config struct {
	Monitoring struct {
		RealTime       bool   `yaml:"realtime"`
		RestoreTimeout string `yaml:"restore_timeout"`
	} `yaml:"monitoring"`

	Notifications struct {
		Enabled bool    `yaml:"enabled"`
		Voice   string  `yaml:"voice"`
		Volume  float64 `yaml:"volume"`
		Rate    int     `yaml:"rate"`
	} `yaml:"notifications"`

	DFU struct {
		WaitTimeout   string `yaml:"wait_timeout"`
		CheckInterval string `yaml:"check_interval"`
	} `yaml:"dfu"`

	USBMonitoring struct {
		CheckInterval   string `yaml:"check_interval"`
		EventBufferSize int    `yaml:"event_buffer_size"`
		CleanupInterval string `yaml:"cleanup_interval"`
	} `yaml:"usb_monitoring"`
}

func loadConfig() (*Config, error) {
	config := &Config{}

	// Устанавливаем значения по умолчанию
	config.Monitoring.RealTime = true
	config.Monitoring.RestoreTimeout = "30m"
	config.Notifications.Enabled = true
	config.Notifications.Voice = "Alex"
	config.Notifications.Volume = 0.7
	config.Notifications.Rate = 200
	config.DFU.WaitTimeout = "60s"
	config.DFU.CheckInterval = "2s"
	config.USBMonitoring.CheckInterval = "1s"
	config.USBMonitoring.EventBufferSize = 100
	config.USBMonitoring.CleanupInterval = "5m"

	// Пытаемся загрузить конфигурацию из файла
	configPaths := []string{
		"config/config.yaml",
		"/usr/local/etc/mac-provisioner/config.yaml",
		"/etc/mac-provisioner/config.yaml",
	}

	for _, path := range configPaths {
		if data, err := ioutil.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(data, config); err != nil {
				log.Printf("Warning: Could not parse config file %s: %v", path, err)
				continue
			}
			log.Printf("Loaded config from: %s", path)
			break
		}
	}

	return config, nil
}

func main() {
	log.Println("Starting Mac Provisioner with real-time device detection...")

	// Загружаем конфигурацию
	config, err := loadConfig()
	if err != nil {
		log.Printf("Warning: Could not load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Инициализируем компоненты
	dfuManager := dfu.NewManager()
	configManager := configurator.NewManager()
	notificationManager := notification.NewManager()
	usbMonitor := usbmonitor.NewMonitor()
	statistics := stats.NewStatistics()

	// Применяем настройки уведомлений
	notificationManager.SetEnabled(config.Notifications.Enabled)
	notificationManager.SetVoice(config.Notifications.Voice)
	notificationManager.SetVolume(config.Notifications.Volume)
	notificationManager.SetRate(config.Notifications.Rate)

	// Оповещение о запуске
	notificationManager.SystemStarted()

	// Запускаем USB мониторинг
	if err := usbMonitor.Start(ctx); err != nil {
		log.Fatalf("Failed to start USB monitor: %v", err)
	}
	defer usbMonitor.Stop()

	// Отслеживаем обрабатываемые устройства
	processedDevices := make(map[string]bool)

	// Обработчик USB событий
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-usbMonitor.Events():
				handleUSBEvent(event, dfuManager, configManager, notificationManager, statistics, processedDevices)
			}
		}
	}()

	// Периодическая очистка списка обработанных устройств
	cleanupInterval, _ := time.ParseDuration(config.USBMonitoring.CleanupInterval)
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanupProcessedDevices(processedDevices, usbMonitor)
			}
		}
	}()

	// Периодический вывод статистики
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				log.Printf("Statistics: %s", statistics.GetSummary())
			}
		}
	}()

	log.Println("Mac Provisioner is running. Press Ctrl+C to stop.")
	<-sigChan
	log.Println("Shutting down...")
	notificationManager.SystemShutdown()
	cancel()

	// Даем время для воспроизведения последнего сообщения
	time.Sleep(2 * time.Second)
}

func handleUSBEvent(event usbmonitor.USBEvent, dfuMgr *dfu.Manager, configMgr *configurator.Manager,
	notifyMgr *notification.Manager, stats *stats.Statistics, processedDevices map[string]bool) {

	switch event.Type {
	case "connected":
		log.Printf("Device connected: %s (%s)", event.Device.SerialNumber, event.Device.Model)

		// Проверяем, нужно ли прошивать устройство
		if !event.Device.IsProvisioned() && !processedDevices[event.Device.SerialNumber] {
			notifyMgr.DeviceDetected(event.Device.SerialNumber, event.Device.Model)
			processedDevices[event.Device.SerialNumber] = true

			go processDevice(event.Device, dfuMgr, configMgr, notifyMgr, stats, func() {
				delete(processedDevices, event.Device.SerialNumber)
			})
		}

	case "disconnected":
		log.Printf("Device disconnected: %s (%s)", event.Device.SerialNumber, event.Device.Model)
		delete(processedDevices, event.Device.SerialNumber)

	case "state_changed":
		log.Printf("Device state changed: %s (%s) - %s",
			event.Device.SerialNumber, event.Device.Model, event.Device.State)

		if !event.Device.IsDFU && event.Device.IsProvisioned() {
			log.Printf("Device %s appears to be provisioned", event.Device.SerialNumber)
		}
	}
}

func cleanupProcessedDevices(processedDevices map[string]bool, usbMonitor *usbmonitor.Monitor) {
	connectedDevices := usbMonitor.GetConnectedDevices()
	connectedMap := make(map[string]bool)

	for _, dev := range connectedDevices {
		connectedMap[dev.SerialNumber] = true
	}

	for serial := range processedDevices {
		if !connectedMap[serial] {
			delete(processedDevices, serial)
			log.Printf("Cleaned up processed device: %s", serial)
		}
	}
}

func processDevice(dev *device.Device, dfuMgr *dfu.Manager, configMgr *configurator.Manager,
	notifyMgr *notification.Manager, stats *stats.Statistics, onComplete func()) {

	defer onComplete()

	log.Printf("Processing device %s", dev.SerialNumber)
	startTime := time.Now()
	stats.DeviceStarted()

	if !dev.IsDFU {
		notifyMgr.EnteringDFUMode(dev.SerialNumber)

		if err := dfuMgr.EnterDFUMode(dev.SerialNumber); err != nil {
			log.Printf("Failed to enter DFU mode for %s: %v", dev.SerialNumber, err)
			notifyMgr.RestoreFailed(dev.SerialNumber, "Failed to enter DFU mode")
			notifyMgr.PlayAlert()
			stats.DeviceCompleted(false, time.Since(startTime))
			return
		}

		notifyMgr.DFUModeEntered(dev.SerialNumber)
		time.Sleep(15 * time.Second)
	}

	notifyMgr.StartingRestore(dev.SerialNumber)

	if err := configMgr.RestoreDevice(dev.SerialNumber, notifyMgr); err != nil {
		log.Printf("Failed to restore device %s: %v", dev.SerialNumber, err)
		notifyMgr.RestoreFailed(dev.SerialNumber, err.Error())
		notifyMgr.PlayAlert()
		stats.DeviceCompleted(false, time.Since(startTime))
		return
	}

	notifyMgr.RestoreCompleted(dev.SerialNumber)
	notifyMgr.PlaySuccess()
	stats.DeviceCompleted(true, time.Since(startTime))
	log.Printf("Successfully restored device %s", dev.SerialNumber)
}
