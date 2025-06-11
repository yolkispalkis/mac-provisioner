package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mac-provisioner/internal/model"
)

// ProvisionResult содержит результат операции прошивки.
type ProvisionResult struct {
	Device *model.Device
	Err    error
}

// runProvisioning выполняет прошивку устройства и отправляет результат в канал.
func runProvisioning(ctx context.Context, dev *model.Device, resultChan chan<- ProvisionResult) {
	displayName := dev.GetDisplayName()
	log.Printf("⚙️  [PROVISION] Начинается прошивка %s", displayName)

	ecid := strings.TrimPrefix(dev.ECID, "0x")
	provCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(provCtx, "cfgutil", "--ecid", ecid, "restore")

	spinnerCtx, spinnerCancel := context.WithCancel(ctx)

	go func() {
		spinnerChars := []string{"|", "/", "-", "\\"}
		i := 0
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-spinnerCtx.Done():
				fmt.Printf("\r%s\n", strings.Repeat(" ", len(displayName)+20))
				return
			case <-ticker.C:
				fmt.Printf("\rПрошивка %s... %s", displayName, spinnerChars[i])
				i = (i + 1) % len(spinnerChars)
			}
		}
	}()

	output, err := cmd.CombinedOutput()

	spinnerCancel()
	time.Sleep(150 * time.Millisecond)

	if err != nil {
		log.Printf("\n❌ Ошибка вывода cfgutil для %s:\n%s", displayName, string(output))
		errMsg := fmt.Errorf("ошибка cfgutil: %w", err)
		resultChan <- ProvisionResult{Device: dev, Err: errMsg}
		return
	}

	log.Printf("✅ [PROVISION] Успешная прошивка %s", displayName)
	resultChan <- ProvisionResult{Device: dev, Err: nil}
}

func triggerDFU(ctx context.Context) {
	log.Println("⚡️ [DFU] Запуск macvdmtool dfu...")
	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ [DFU] macvdmtool завершился с ошибкой: %v", err)
	}
}

func isDFUPort(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}
	base := strings.Split(usbLocation, "/")[0]
	return strings.HasPrefix(base, "0x011") || strings.HasPrefix(base, "0x001")
}

// ==================================================================================
// === НОВАЯ ФУНКЦИЯ ОЧИСТКИ КЕША ===
// ==================================================================================
func cleanupConfiguratorCache() {
	log.Println("🧹 [CLEANUP] Попытка очистки кеша Apple Configurator...")

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("🧹 [CLEANUP] ❌ Не удалось определить домашнюю директорию: %v", err)
		return
	}

	cachePath := filepath.Join(homeDir, "Library", "Containers", "com.apple.configurator.xpc.DeviceService", "Data", "tmp", "TemporaryItems")

	entries, err := os.ReadDir(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("🧹 [CLEANUP] Директория кеша не найдена, очистка не требуется.")
			return
		}
		log.Printf("🧹 [CLEANUP] ❌ Ошибка чтения директории кеша %s: %v", cachePath, err)
		return
	}

	if len(entries) == 0 {
		log.Printf("🧹 [CLEANUP] Кеш уже пуст.")
		return
	}

	var itemsDeleted int
	for _, entry := range entries {
		fullPath := filepath.Join(cachePath, entry.Name())
		log.Printf("🧹 [CLEANUP] Удаление: %s", fullPath)
		if err := os.RemoveAll(fullPath); err != nil {
			log.Printf("🧹 [CLEANUP] ❌ Ошибка удаления %s: %v", fullPath, err)
		} else {
			itemsDeleted++
		}
	}

	log.Printf("🧹 [CLEANUP] ✅ Очистка завершена. Удалено элементов: %d.", itemsDeleted)
}
