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
	displayName := dev.GetDisplayName()
	log.Printf("⚙️  [PROVISION] Начинается прошивка %s", displayName)

	ecid := strings.TrimPrefix(dev.ECID, "0x")
	provCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(provCtx, "cfgutil", "--ecid", ecid, "restore")

	// Контекст для спиннера, который отменится, когда прошивка закончится
	spinnerCtx, spinnerCancel := context.WithCancel(ctx)

	// Запускаем спиннер в отдельной горутине
	go func() {
		// \r (carriage return) возвращает курсор в начало строки,
		// позволяя переписывать ее и создавать эффект анимации.
		spinnerChars := []string{"|", "/", "-", "\\"}
		i := 0
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-spinnerCtx.Done():
				// Очищаем строку со спиннером перед выходом
				fmt.Printf("\r%s\n", strings.Repeat(" ", len(displayName)+20))
				return
			case <-ticker.C:
				fmt.Printf("\rПрошивка %s... %s", displayName, spinnerChars[i])
				i = (i + 1) % len(spinnerChars)
			}
		}
	}()

	output, err := cmd.CombinedOutput()

	// Останавливаем спиннер
	spinnerCancel()
	// Небольшая пауза, чтобы спиннер успел очистить строку
	time.Sleep(150 * time.Millisecond)

	if err != nil {
		// Выводим ошибку на новой строке, чтобы не смешивать со спиннером
		log.Printf("\n❌ Ошибка вывода cfgutil для %s:\n%s", displayName, string(output))
		errMsg := fmt.Errorf("ошибка cfgutil: %w", err)
		resultChan <- ProvisionResult{Device: dev, Err: errMsg}
		return
	}

	log.Printf("✅ [PROVISION] Успешная прошивка %s", displayName)
	resultChan <- ProvisionResult{Device: dev, Err: nil}
}

// triggerDFU и isDFUPort остаются без изменений
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
