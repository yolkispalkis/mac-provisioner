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

/*=====================================================================
  STRUCT
  =====================================================================*/

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

/*=====================================================================
  PUBLIC — ОСНОВНАЯ ОБРАБОТКА
  =====================================================================*/

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	/*----- защита от параллельной обработки одного и того же -----*/
	m.processingMu.Lock()
	if m.processing[dev.SerialNumber] {
		m.processingMu.Unlock()
		return
	}
	m.processing[dev.SerialNumber] = true
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, dev.SerialNumber)
		m.processingMu.Unlock()
	}()

	start := time.Now()
	m.stats.DeviceStarted()
	targetID := dev.SerialNumber

	/*------------------------ DFU ------------------------*/
	if !dev.IsDFU {
		m.notifier.EnteringDFUMode(dev.SerialNumber)
		log.Printf("📱 Переводим %s в DFU…", dev.SerialNumber)

		if err := m.dfuManager.EnterDFUMode(dev.SerialNumber); err != nil {
			m.handleDFUError(ctx, dev, start, err)
			return
		}
	}

	/* если уже DFU → ECID */
	if dev.IsDFU && dev.ECID != "" {
		targetID = dev.ECID
	}

	/*--------------------- RESTORE ---------------------*/
	m.notifier.StartingRestore(dev.SerialNumber)
	if err := m.restoreDevice(ctx, targetID, dev.SerialNumber); err != nil {
		log.Printf("❌ restore error: %v", err)
		m.notifier.RestoreFailed(dev.SerialNumber, err.Error())
		m.notifier.PlayAlert()
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	m.notifier.RestoreCompleted(dev.SerialNumber)
	m.notifier.PlaySuccess()
	m.stats.DeviceCompleted(true, time.Since(start))
	log.Printf("✅ %s восстановлен за %v", dev.SerialNumber, time.Since(start).Round(time.Second))
}

/*=====================================================================
  DFU ERROR HANDLER
  =====================================================================*/

func (m *Manager) handleDFUError(
	ctx context.Context, dev *device.Device, started time.Time, err error,
) {
	log.Printf("❌ DFU error: %v", err)

	// Требуется ручной DFU
	if strings.Contains(err.Error(), "ручного") {
		m.notifier.ManualDFURequired(dev.SerialNumber)
		m.notifier.WaitingForDFU(dev.SerialNumber)

		if ecid := m.waitForManualDFU(ctx, 60*time.Second); ecid != "" {
			dev.ECID = ecid
			dev.IsDFU = true
			return // продолжаем workflow — теперь устройство в DFU
		}
	}

	// Любая другая ошибка = отказ
	m.notifier.RestoreFailed(dev.SerialNumber, err.Error())
	m.notifier.PlayAlert()
	m.stats.DeviceCompleted(false, time.Since(started))
}

/*=====================================================================
  RESTORE (cfgutil)
  =====================================================================*/

func (m *Manager) restoreDevice(
	ctx context.Context, identifier string, originalSerial string,
) error {
	log.Printf("🔧 restore start: identifier=%s", identifier)

	// 1. Нормализуем идентификатор
	useECID := false
	id := strings.TrimPrefix(strings.ToLower(identifier), "dfu-")

	if strings.HasPrefix(id, "0x") {
		dec, err := hexToDec(id)
		if err != nil {
			return fmt.Errorf("ECID %s convert: %w", id, err)
		}
		id = dec
	}
	if isDigits(id) {
		useECID = true
	}

	// 2. Формируем команду cfgutil
	var cmd *exec.Cmd
	if useECID {
		//  ⚠️  опция --ecid ставится ПЕРЕД restore
		cmd = exec.Command("cfgutil", "--ecid", id, "restore")
	} else {
		cmd = exec.Command("cfgutil", "-s", id, "restore")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cfgutil restore: %w\n%s", err, string(out))
	}

	// 3. Ждём окончания
	return m.waitForRestoreCompletion(ctx, id, originalSerial)
}

/*=====================================================================
  WAIT HELPERS
  =====================================================================*/

// ждём, пока пользователь вручную введёт Мак в DFU
func (m *Manager) waitForManualDFU(ctx context.Context, timeout time.Duration) string {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	deadline := time.After(timeout)

	for {
		select {
		case <-ctx.Done():
			return ""
		case <-deadline:
			return ""
		case <-ticker.C:
			if list := m.dfuManager.GetDFUDevices(); len(list) > 0 {
				return list[0].ECID
			}
		}
	}
}

/*=====================================================================
  STATUS POLLING
  =====================================================================*/

func (m *Manager) waitForRestoreCompletion(
	ctx context.Context, identifier string, originalSerial string,
) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	timeout := time.After(30 * time.Minute)
	prev := ""

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("операция отменена")
		case <-timeout:
			return fmt.Errorf("таймаут восстановления %s", identifier)
		case <-ticker.C:
			status, err := m.getDeviceStatus(identifier)
			if err != nil {
				continue
			}

			if status != prev && status != "Устройство не найдено" {
				m.notifier.RestoreProgress(originalSerial, m.getReadableStatus(status))
				prev = status
			}
			if m.isRestoreComplete(status) {
				return nil
			}
		}
	}
}

func (m *Manager) getDeviceStatus(identifier string) (string, error) {
	out, err := exec.Command("cfgutil", "list").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, identifier) {
			return line, nil
		}
	}
	return "Устройство не найдено", nil
}

/*=====================================================================
  STATUS HELPERS
  =====================================================================*/

func (m *Manager) getReadableStatus(s string) string {
	l := strings.ToLower(s)
	switch {
	case strings.Contains(l, "dfu"), strings.Contains(l, "recovery"):
		return "в режиме восстановления"
	case strings.Contains(l, "restoring"):
		return "восстанавливает прошивку"
	case strings.Contains(l, "available"):
		return "доступно"
	case strings.Contains(l, "paired"):
		return "сопряжено и готово"
	default:
		return "неизвестный статус"
	}
}

func (m *Manager) isRestoreComplete(status string) bool {
	l := strings.ToLower(status)
	return !strings.Contains(l, "dfu") &&
		!strings.Contains(l, "recovery") &&
		!strings.Contains(l, "restoring") &&
		(strings.Contains(l, "available") || strings.Contains(l, "paired"))
}

/*=====================================================================
  UTILITIES
  =====================================================================*/

// hex "0x1A2B" → "6699"
func hexToDec(h string) (string, error) {
	h = strings.TrimPrefix(h, "0x")
	v, err := strconv.ParseUint(h, 16, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(v, 10), nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
