package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/notifier"
	"mac-provisioner/internal/orchestrator"
)

func main() {
	log.SetFlags(log.Ltime)
	log.Println("🚀 Запуск Mac Provisioner (v2, архитектура на каналах)")

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("❌ Не удалось загрузить конфигурацию: %v", err)
	}

	voiceNotifier := notifier.New(
		cfg.Notifications.Enabled,
		cfg.Notifications.Voice,
		cfg.Notifications.Rate,
	)

	app := orchestrator.New(cfg, voiceNotifier)

	ctx, cancel := context.WithCancel(context.Background())

	// Обработка сигнала завершения для graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("🛑 Получен сигнал завершения, останавливаемся...")
		cancel()
	}()

	// Блокирующий вызов, который запускает всю логику приложения
	app.Start(ctx)

	log.Println("✅ Программа завершена.")
}
