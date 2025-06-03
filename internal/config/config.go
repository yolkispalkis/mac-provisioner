package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Monitoring    MonitoringConfig   `yaml:"monitoring"`
	Notifications NotificationConfig `yaml:"notifications"`
}

type MonitoringConfig struct {
	CheckInterval   time.Duration `yaml:"check_interval"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
}

type NotificationConfig struct {
	Enabled bool    `yaml:"enabled"`
	Voice   string  `yaml:"voice"`
	Rate    int     `yaml:"rate"`
	Volume  float64 `yaml:"volume"`
}

func Load() (*Config, error) {
	cfg := &Config{
		Monitoring: MonitoringConfig{
			CheckInterval:   3 * time.Second,
			CleanupInterval: 30 * time.Second,
		},
		Notifications: NotificationConfig{
			Enabled: true,
			Voice:   "Milena",
			Rate:    200,
			Volume:  0.8,
		},
	}

	if data, err := os.ReadFile("config.yaml"); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("ошибка парсинга конфигурации: %w", err)
		}
	}

	return cfg, nil
}
