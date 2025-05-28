package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Monitoring    MonitoringConfig   `yaml:"monitoring"`
	Notifications NotificationConfig `yaml:"notifications"`
	DFU           DFUConfig          `yaml:"dfu"`
	Provisioning  ProvisioningConfig `yaml:"provisioning"`
}

type MonitoringConfig struct {
	CheckInterval   time.Duration `yaml:"check_interval"`
	EventBufferSize int           `yaml:"event_buffer_size"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
}

type NotificationConfig struct {
	Enabled bool    `yaml:"enabled"`
	Voice   string  `yaml:"voice"`
	Volume  float64 `yaml:"volume"`
	Rate    int     `yaml:"rate"`
}

type DFUConfig struct {
	WaitTimeout   time.Duration `yaml:"wait_timeout"`
	CheckInterval time.Duration `yaml:"check_interval"`
}

type ProvisioningConfig struct {
	RestoreTimeout time.Duration `yaml:"restore_timeout"`
	MaxRetries     int           `yaml:"max_retries"`
}

func Load() (*Config, error) {
	cfg := defaultConfig()

	configPaths := []string{
		"configs/config.yaml",
		"/usr/local/etc/mac-provisioner/config.yaml",
		"/etc/mac-provisioner/config.yaml",
	}

	for _, path := range configPaths {
		if data, err := os.ReadFile(path); err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("ошибка парсинга конфигурации %s: %w", path, err)
			}
			fmt.Printf("Загружена конфигурация из: %s\n", path)
			return cfg, nil
		}
	}

	fmt.Println("Используется конфигурация по умолчанию")
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Monitoring: MonitoringConfig{
			CheckInterval:   500 * time.Millisecond,
			EventBufferSize: 100,
			CleanupInterval: 5 * time.Minute,
		},
		Notifications: NotificationConfig{
			Enabled: true,
			Voice:   "Milena", // Русский голос
			Volume:  0.7,
			Rate:    180,
		},
		DFU: DFUConfig{
			WaitTimeout:   2 * time.Minute,
			CheckInterval: 2 * time.Second,
		},
		Provisioning: ProvisioningConfig{
			RestoreTimeout: 30 * time.Minute,
			MaxRetries:     3,
		},
	}
}
