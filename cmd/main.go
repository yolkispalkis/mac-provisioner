package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/notifier"
	"mac-provisioner/internal/orchestrator"
)

func main() {
	infoLogger := log.New(os.Stdout, "", log.Ltime)

	var debugOutput io.Writer = io.Discard
	if os.Getenv("DEBUG") == "1" {
		debugOutput = os.Stdout
	}
	debugLogger := log.New(debugOutput, "DEBUG: ", log.Ltime)

	infoLogger.Println("Запуск Mac Provisioner")
	if os.Getenv("DEBUG") == "1" {
		infoLogger.Println("   (Режим отладки включен)")
	}

	cfg, err := config.Load("config.yaml")
	if err != nil {
		infoLogger.Fatalf("[FATAL] Не удалось загрузить конфигурацию: %v", err)
	}

	voiceNotifier := notifier.New(
		cfg.Notifications.Enabled,
		cfg.Notifications.Voice,
		cfg.Notifications.Rate,
	)

	app := orchestrator.New(cfg, voiceNotifier, infoLogger, debugLogger)

	ctx, cancel := context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		infoLogger.Println("Получен сигнал завершения, остановка...")
		cancel()
	}()

	app.Start(ctx)

	infoLogger.Println("Программа завершена.")
}
