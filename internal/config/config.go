package config

import (
	"log"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	CheckInterval     time.Duration `yaml:"check_interval"`
	DFUCooldown       time.Duration `yaml:"dfu_cooldown"`
	MaxConcurrentJobs int           `yaml:"max_concurrent_jobs"`
	Notifications     struct {
		Enabled bool   `yaml:"enabled"`
		Voice   string `yaml:"voice"`
		Rate    int    `yaml:"rate"`
	} `yaml:"notifications"`
}

func Load(path string) (*Config, error) {
	// Значения по умолчанию
	cfg := &Config{
		CheckInterval:     3 * time.Second,
		DFUCooldown:       1 * time.Hour,
		MaxConcurrentJobs: 10,
	}
	cfg.Notifications.Enabled = true
	cfg.Notifications.Voice = "Milena"
	cfg.Notifications.Rate = 200

	data, err := os.ReadFile(path)
	if err != nil {
		// Если файл не найден, это не ошибка - используем значения по умолчанию.
		if os.IsNotExist(err) {
			log.Printf("Файл конфигурации '%s' не найден, используются значения по умолчанию.", path)
			return cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
