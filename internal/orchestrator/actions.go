package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os/exec"
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
	log.Printf("⚙️  [PROVISION] Начинается прошивка %s", dev.GetDisplayName())

	// cfgutil требует ECID без префикса "0x"
	ecid := strings.TrimPrefix(dev.ECID, "0x")

	// Таймаут на всю операцию
	provCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(provCtx, "cfgutil", "--ecid", ecid, "restore")
	output, err := cmd.CombinedOutput()

	if err != nil {
		errMsg := fmt.Errorf("ошибка cfgutil: %w, вывод: %s", err, string(output))
		resultChan <- ProvisionResult{Device: dev, Err: errMsg}
		return
	}

	log.Printf("✅ [PROVISION] Успешная прошивка %s", dev.GetDisplayName())
	resultChan <- ProvisionResult{Device: dev, Err: nil}
}

// triggerDFU выполняет команду для перевода в DFU режим.
func triggerDFU(ctx context.Context) {
	log.Println("⚡️ [DFU] Запуск macvdmtool dfu...")
	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		// Не фатальная ошибка, просто логируем, так как это попытка
		log.Printf("⚠️ [DFU] macvdmtool завершился с ошибкой: %v", err)
	}
}

// isDFUPort проверяет, является ли порт подходящим для автоматического DFU.
func isDFUPort(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}
	// На Mac Studio DFU-порт - это 0x01100000. На других Mac может быть 0x00100000.
	// Обобщенная проверка.
	base := strings.Split(usbLocation, "/")[0]
	return strings.HasPrefix(base, "0x011") || strings.HasPrefix(base, "0x001")
}
