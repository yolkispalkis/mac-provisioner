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
}

type MonitoringConfig struct {
	CheckInterval   time.Duration `yaml:"check_interval"`
	CleanupInterval time.Duration `yaml:"cleanup_interval"`
	EventBufferSize int           `yaml:"event_buffer_size"`
}

type NotificationConfig struct {
	Enabled bool    `yaml:"enabled"`
	Voice   string  `yaml:"voice"`
	Rate    int     `yaml:"rate"`
	Volume  float64 `yaml:"volume"`
}

func Load() (*Config, error) {
	// Значения по умолчанию
	cfg := &Config{
		Monitoring: MonitoringConfig{
			CheckInterval:   3 * time.Second,
			CleanupInterval: 30 * time.Second,
			EventBufferSize: 100,
		},
		Notifications: NotificationConfig{
			Enabled: true,
			Voice:   "Milena",
			Rate:    200,
			Volume:  0.8,
		},
	}

	// Попытка загрузить конфигурацию из файла
	configPath := "config.yaml"
	if _, err := os.Stat(configPath); err == nil {
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("ошибка чтения файла конфигурации: %w", err)
		}

		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("ошибка парсинга YAML конфигурации: %w", err)
		}
	}

	return cfg, nil
}
