package orchestrator

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/model"
)

type ProvisionStatusType string

const (
	StatusDownloading ProvisionStatusType = "Загрузка"
	StatusPreparing   ProvisionStatusType = "Подготовка"
	StatusRestoring   ProvisionStatusType = "Прошивка"
	StatusVerifying   ProvisionStatusType = "Проверка"
)

type ProvisionUpdate struct {
	Device     *model.Device
	Status     ProvisionStatusType
	Percentage string
}

type ProvisionResult struct {
	Device *model.Device
	Err    error
}

func runProvisioning(ctx context.Context, dev *model.Device, resultChan chan<- ProvisionResult, updateChan chan<- ProvisionUpdate, infoLogger *log.Logger) {
	displayName := dev.GetDisplayName()
	infoLogger.Printf("[PROVISION] Начинается прошивка %s", displayName)

	ecid := strings.TrimPrefix(dev.ECID, "0x")
	provCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(provCtx, "cfgutil", "--ecid", ecid, "restore")

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		infoLogger.Printf("[ERROR] Ошибка запуска cfgutil: %v", err)
		resultChan <- ProvisionResult{Device: dev, Err: err}
		return
	}

	var wg sync.WaitGroup
	var lastStatus ProvisionStatusType
	var outputCollector strings.Builder
	percentRegex := regexp.MustCompile(`\[\s*(\d{1,3}(?:\.\d{1,2})?)\s*%`)

	processStream := func(stream io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			line := scanner.Text()
			outputCollector.WriteString(line + "\n")

			var currentStatus ProvisionStatusType
			lowerLine := strings.ToLower(line)
			if strings.Contains(lowerLine, "downloading") {
				currentStatus = StatusDownloading
			} else if strings.Contains(lowerLine, "preparing") {
				currentStatus = StatusPreparing
			} else if strings.Contains(lowerLine, "restoring") {
				currentStatus = StatusRestoring
			} else if strings.Contains(lowerLine, "verifying") {
				currentStatus = StatusVerifying
			}

			if currentStatus != "" && currentStatus != lastStatus {
				lastStatus = currentStatus
				updateChan <- ProvisionUpdate{Device: dev, Status: currentStatus}
			}

			if matches := percentRegex.FindStringSubmatch(line); len(matches) > 1 {
				percentStr := matches[1]
				updateChan <- ProvisionUpdate{Device: dev, Percentage: percentStr}
			}
		}
	}

	wg.Add(2)
	go processStream(stdout)
	go processStream(stderr)

	wg.Wait()
	err := cmd.Wait()

	if err != nil {
		infoLogger.Printf("[ERROR] Процесс cfgutil для %s завершился с ошибкой: %v", displayName, err)
		infoLogger.Printf("[ERROR] Полный вывод cfgutil для %s:\n%s", displayName, outputCollector.String())
		resultChan <- ProvisionResult{Device: dev, Err: err}
		return
	}

	infoLogger.Printf("[PROVISION] Успешная прошивка %s", displayName)
	resultChan <- ProvisionResult{Device: dev, Err: nil}
}

func triggerDFU(ctx context.Context, infoLogger *log.Logger) {
	infoLogger.Println("[DFU] Запуск macvdmtool dfu...")
	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		infoLogger.Printf("[WARN] macvdmtool завершился с ошибкой: %v", err)
	}
}

func isDFUPort(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}
	base := strings.Split(usbLocation, "/")[0]
	return strings.HasPrefix(base, "0x011") || strings.HasPrefix(base, "0x001")
}

func cleanupConfiguratorCache(infoLogger, debugLogger *log.Logger) {
	infoLogger.Println("[CLEANUP] Попытка очистки кеша Apple Configurator...")

	homeDir, err := os.UserHomeDir()
	if err != nil {
		infoLogger.Printf("[CLEANUP][ERROR] Не удалось определить домашнюю директорию: %v", err)
		return
	}

	cachePath := filepath.Join(homeDir, "Library", "Containers", "com.apple.configurator.xpc.DeviceService", "Data", "tmp", "TemporaryItems")

	entries, err := os.ReadDir(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			infoLogger.Printf("[CLEANUP] Директория кеша не найдена, очистка не требуется.")
			return
		}
		infoLogger.Printf("[CLEANUP][ERROR] Ошибка чтения директории кеша %s: %v", cachePath, err)
		return
	}

	if len(entries) == 0 {
		infoLogger.Printf("[CLEANUP] Кеш уже пуст.")
		return
	}

	var itemsDeleted int
	for _, entry := range entries {
		fullPath := filepath.Join(cachePath, entry.Name())
		debugLogger.Printf("[CLEANUP] Удаление: %s", fullPath)
		if err := os.RemoveAll(fullPath); err != nil {
			infoLogger.Printf("[CLEANUP][ERROR] Ошибка удаления %s: %v", fullPath, err)
		} else {
			itemsDeleted++
		}
	}

	infoLogger.Printf("[CLEANUP] Очистка завершена. Удалено элементов: %d.", itemsDeleted)
}
