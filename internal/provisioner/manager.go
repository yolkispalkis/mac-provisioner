package provisioner

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
	"mac-provisioner/internal/stats"
)

/*
=====================================================================

	STRUCT (без изменений)
	=====================================================================
*/
type Manager struct {
	dfuManager   *dfu.Manager
	notifier     *notification.Manager
	stats        *stats.Manager
	processing   map[string]bool
	processingMu sync.RWMutex
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager, stats *stats.Manager) *Manager {
	return &Manager{
		dfuManager: dfuMgr,
		notifier:   notifier,
		stats:      stats,
		processing: make(map[string]bool),
	}
}

/*
=====================================================================

	PUBLIC — ОСНОВНАЯ ОБРАБОТКА
	=====================================================================
*/
func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	m.processingMu.Lock()
	if m.processing[dev.SerialNumber] {
		m.processingMu.Unlock()
		log.Printf("ℹ️ Процесс прошивки для %s уже запущен, пропускаем.", dev.SerialNumber)
		return
	}
	m.processing[dev.SerialNumber] = true
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, dev.SerialNumber)
		m.processingMu.Unlock()
	}()

	log.Printf("🚀 Начало процесса прошивки для устройства: %s (Модель: %s, ECID: %s)",
		dev.GetFriendlyName(), dev.Model, dev.ECID)

	start := time.Now()
	m.stats.DeviceStarted()
	// targetECID будет инициализирован после проверок

	if !dev.IsDFU {
		log.Printf("Критическая ошибка логики: ProcessDevice вызван для устройства %s не в DFU.", dev.GetFriendlyName())
		m.notifier.RestoreFailed(dev, "Внутренняя ошибка: устройство не в DFU")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	if dev.ECID == "" {
		log.Printf("❌ Ошибка: Устройство %s в DFU, но ECID отсутствует. Прошивка невозможна.", dev.GetFriendlyName())
		m.notifier.RestoreFailed(dev, "ECID отсутствует у DFU устройства")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	// Теперь инициализируем targetECID, так как проверки пройдены
	targetECID := dev.ECID

	log.Printf("⚙️ Начало восстановления для DFU устройства %s (ECID: %s)", dev.GetFriendlyName(), targetECID)
	m.notifier.StartingRestore(dev)

	decimalECID, err := normalizeECIDForCfgutil(targetECID) // Используем targetECID
	if err != nil {
		log.Printf("❌ Не удалось нормализовать ECID %s: %v", targetECID, err)
		m.notifier.RestoreFailed(dev, fmt.Sprintf("Неверный формат ECID: %s", targetECID))
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	log.Printf("ℹ️ Нормализованный ECID для cfgutil: %s", decimalECID)

	restoreCmdCtx, cancelRestoreCmd := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelRestoreCmd()

	cmd := exec.CommandContext(restoreCmdCtx, "cfgutil", "--ecid", decimalECID, "restore")
	log.Printf("⏳ Выполнение: cfgutil --ecid %s restore...", decimalECID)
	restoreOutput, restoreErr := cmd.CombinedOutput()

	if restoreErr != nil {
		if restoreCmdCtx.Err() == context.DeadlineExceeded {
			log.Printf("❌ Таймаут выполнения 'cfgutil restore' для ECID %s.", decimalECID)
			errMsg := fmt.Sprintf("cfgutil restore: таймаут (%v)", 15*time.Minute)
			m.notifier.RestoreFailed(dev, errMsg)
		} else {
			log.Printf("❌ Ошибка выполнения 'cfgutil restore' для ECID %s: %v", decimalECID, restoreErr)
			log.Printf("Output:\n%s", string(restoreOutput))
			errMsg := fmt.Sprintf("cfgutil restore: %s. %s", restoreErr, небольшаяЧасть(string(restoreOutput), 100))
			m.notifier.RestoreFailed(dev, errMsg)
		}
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	log.Printf("✅ Команда 'cfgutil restore' для ECID %s завершилась успешно (по данным самой команды).", decimalECID)

	log.Println("⏳ Ожидание выхода устройства из DFU режима после restore (до 30 секунд)...")
	postRestoreWaitCtx, postRestoreWaitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer postRestoreWaitCancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	deviceExitedDFU := false

LoopPostRestore:
	for {
		select {
		case <-postRestoreWaitCtx.Done():
			log.Printf("⚠️ Таймаут ожидания выхода устройства ECID %s из DFU режима.", decimalECID)
			break LoopPostRestore
		case <-ticker.C:
			found := false
			currentDfuDevs := m.dfuManager.GetDFUDevices(postRestoreWaitCtx)
			for _, dfuDev := range currentDfuDevs {
				normEcidCurrent, _ := normalizeECIDForCfgutil(dfuDev.ECID)
				if normEcidCurrent == decimalECID {
					found = true
					break
				}
			}
			if !found {
				deviceExitedDFU = true
				break LoopPostRestore
			}
			log.Printf("... устройство ECID %s все еще в DFU, ждем...", decimalECID)
		case <-ctx.Done():
			log.Println("ℹ️ Основной контекст отменен во время ожидания выхода из DFU.")
			m.stats.DeviceCompleted(false, time.Since(start))
			return
		}
	}

	if !deviceExitedDFU {
		log.Printf("⚠️ Устройство ECID %s все еще в DFU после 'cfgutil restore' и ожидания. Возможно, прошивка не удалась или требует больше времени на перезагрузку.", decimalECID)
		m.notifier.RestoreFailed(dev, "Устройство осталось в DFU после restore")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	// Используем targetECID для лога, так как он содержит исходное значение ECID из dev.ECID
	log.Printf("🎉 Восстановление для %s (ECID: %s) успешно завершено (устройство вышло из DFU).", dev.GetFriendlyName(), targetECID)
	m.notifier.RestoreCompleted(dev)
	m.stats.DeviceCompleted(true, time.Since(start))
}

/*
=====================================================================

	UTILITIES (без изменений)
	=====================================================================
*/
func hexToDec(hexStr string) (string, error) {
	cleanHex := strings.ToLower(strings.TrimPrefix(hexStr, "0x"))
	val, err := strconv.ParseUint(cleanHex, 16, 64)
	if err != nil {
		return "", fmt.Errorf("ошибка парсинга HEX '%s': %w", hexStr, err)
	}
	return strconv.FormatUint(val, 10), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizeECIDForCfgutil(ecidStr string) (string, error) {
	if ecidStr == "" {
		return "", fmt.Errorf("ECID не может быть пустым")
	}
	if isDigits(ecidStr) {
		return ecidStr, nil
	}
	if strings.HasPrefix(strings.ToLower(ecidStr), "0x") || !isDigits(ecidStr) {
		return hexToDec(ecidStr)
	}
	return "", fmt.Errorf("не удалось определить формат ECID для конвертации: %s", ecidStr)
}

func небольшаяЧасть(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
