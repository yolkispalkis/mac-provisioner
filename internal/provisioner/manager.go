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

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	/* ---------- защита от повторной обработки ---------- */
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

	log.Printf("🔄 Начинается обработка устройства %s (%s)", dev.SerialNumber, dev.Model)
	startTime := time.Now()
	m.stats.DeviceStarted()

	targetIdentifier := dev.SerialNumber

	/* ---------- DFU ---------- */
	if !dev.IsDFU {
		m.notifier.EnteringDFUMode(dev.SerialNumber)
		log.Printf("📱 Попытка перевода в DFU режим для устройства %s", dev.SerialNumber)

		if err := m.dfuManager.EnterDFUMode(dev.SerialNumber); err != nil {
			log.Printf("❌ Ошибка перехода в DFU режим: %v", err)
			if strings.Contains(err.Error(), "ручного входа") {
				m.notifier.ManualDFURequired(dev.SerialNumber)
				m.notifier.WaitingForDFU(dev.SerialNumber)
				if ecid := m.waitForManualDFU(ctx, 60); ecid != "" {
					targetIdentifier = ecid
				} else {
					m.fail(dev.SerialNumber, startTime, "Устройство не перешло в DFU")
					return
				}
			} else {
				m.fail(dev.SerialNumber, startTime, err.Error())
				return
			}
		} else {
			if ecid := m.dfuManager.GetFirstDFUECID(); ecid != "" {
				targetIdentifier = ecid
			}
		}
	} else if dev.ECID != "" { // уже DFU
		targetIdentifier = dev.ECID
	}

	/* ---------- RESTORE ---------- */
	m.notifier.StartingRestore(dev.SerialNumber)
	log.Printf("🔧 Начинается восстановление для %s (id: %s)", dev.SerialNumber, targetIdentifier)

	if err := m.restoreDevice(ctx, targetIdentifier, dev.SerialNumber); err != nil {
		log.Printf("❌ Ошибка восстановления %s: %v", targetIdentifier, err)
		m.notifier.RestoreFailed(dev.SerialNumber, err.Error())
		m.notifier.PlayAlert()
		m.stats.DeviceCompleted(false, time.Since(startTime))
		return
	}

	m.notifier.RestoreCompleted(dev.SerialNumber)
	m.notifier.PlaySuccess()
	m.stats.DeviceCompleted(true, time.Since(startTime))
	log.Printf("✅ Успешно восстановлено %s за %v", dev.SerialNumber, time.Since(startTime).Round(time.Second))
}

/* ====================================================================
   RESTORE SECTION
   ==================================================================== */

func (m *Manager) restoreDevice(ctx context.Context, identifier, originalSerial string) error {
	log.Printf("Начинается восстановление для идентификатора %s…", identifier)

	var cmd *exec.Cmd

	/* --- определяем формат идентификатора --- */
	if strings.HasPrefix(strings.ToLower(identifier), "dfu-") {
		identifier = strings.TrimPrefix(strings.ToLower(identifier), "dfu-")
	}
	if strings.HasPrefix(strings.ToLower(identifier), "0x") {
		// cfgutil ожидает ECID в ДЕСЯТИЧНОМ формате → конвертируем
		dec, err := hexToDec(identifier)
		if err != nil {
			return fmt.Errorf("не удалось конвертировать ECID %s: %w", identifier, err)
		}
		log.Printf("ℹ️  ECID %s → десятичный %s", identifier, dec)
		identifier = dec
	}

	/* --- формируем команду --- */
	if isDigits(identifier) {
		cmd = exec.Command("cfgutil", "restore", "-e", identifier, "--erase")
	} else {
		cmd = exec.Command("cfgutil", "restore", "-s", identifier, "--erase")
	}

	/* --- выполняем --- */
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cfgutil restore завершился с ошибкой: %w (%s)", err, string(out))
	}

	return m.waitForRestoreCompletion(ctx, identifier, originalSerial)
}

/* ====================================================================
   HELPERS
   ==================================================================== */

// hexToDec("0x1A2B") -> "6699"
func hexToDec(hexStr string) (string, error) {
	hexStr = strings.TrimPrefix(strings.ToLower(hexStr), "0x")
	value, err := strconv.ParseUint(hexStr, 16, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(value, 10), nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

/* ---------- ожидание ручного DFU ---------- */

func (m *Manager) waitForManualDFU(ctx context.Context, seconds int) string {
	for i := 0; i < seconds/2; i++ {
		select {
		case <-ctx.Done():
			return ""
		default:
		}
		time.Sleep(2 * time.Second)

		if devs := m.dfuManager.GetDFUDevices(); len(devs) > 0 {
			return devs[0].ECID
		}
		if i%5 == 0 {
			log.Printf("⏳ Ожидаем DFU… %d/%d с", i*2, seconds)
		}
	}
	return ""
}

/* ---------- завершение/статистика при ошибке ---------- */

func (m *Manager) fail(serial string, started time.Time, errMsg string) {
	m.notifier.RestoreFailed(serial, errMsg)
	m.notifier.PlayAlert()
	m.stats.DeviceCompleted(false, time.Since(started))
}

/* ====================================================================
   ОСТАВШИЕСЯ МЕТОДЫ (getDeviceStatus, waitForRestoreCompletion и т.д.)
   --------------------------------------------------------------------
   Ниже код не изменялся; оставлен как был.
   ==================================================================== */

func (m *Manager) waitForRestoreCompletion(ctx context.Context, identifier, originalSerial string) error {
	/* оригинальная логика … */
	maxWait := 30 * time.Minute
	interval := 30 * time.Second

	timeout := time.After(maxWait)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastStatus := ""

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

			if status != lastStatus && status != "Устройство не найдено" {
				m.notifier.RestoreProgress(originalSerial, m.getReadableStatus(status))
				lastStatus = status
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

/* ----------- reading status helpers (не изменялись) ----------- */

func (m *Manager) getReadableStatus(status string) string {
	s := strings.ToLower(status)
	switch {
	case strings.Contains(s, "dfu"), strings.Contains(s, "recovery"):
		return "в режиме восстановления"
	case strings.Contains(s, "restoring"):
		return "восстанавливает прошивку"
	case strings.Contains(s, "available"):
		return "доступно"
	case strings.Contains(s, "paired"):
		return "сопряжено и готово"
	default:
		return "неизвестный статус"
	}
}

func (m *Manager) isRestoreComplete(status string) bool {
	s := strings.ToLower(status)
	return !strings.Contains(s, "dfu") &&
		!strings.Contains(s, "recovery") &&
		!strings.Contains(s, "restoring") &&
		(strings.Contains(s, "available") || strings.Contains(s, "paired"))
}
