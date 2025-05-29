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
   ──────────────────────────────────────────────────────────
   STRUCT
   ──────────────────────────────────────────────────────────
*/

type Manager struct {
	dfuManager   *dfu.Manager
	notifier     *notification.Manager
	stats        *stats.Manager
	processing   map[string]bool // key = Device.UniqueID()
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
   ──────────────────────────────────────────────────────────
   PUBLIC  –  ОСНОВНАЯ ОБРАБОТКА
   ──────────────────────────────────────────────────────────
*/

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	uid := dev.UniqueID()

	// ► Не допускаем параллельной обработки одного и того же устройства
	m.processingMu.Lock()
	if m.processing[uid] {
		m.processingMu.Unlock()
		log.Printf("ℹ️ Процесс прошивки для %s уже запущен, пропускаем.", dev.GetFriendlyName())
		return
	}
	m.processing[uid] = true
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, uid)
		m.processingMu.Unlock()
	}()

	log.Printf("🚀 Старт прошивки: %s (ECID: %s, USB: %s)",
		dev.GetFriendlyName(), dev.ECID, dev.USBLocation)

	start := time.Now()
	m.stats.DeviceStarted()

	// ── Проверки ──────────────────────────────────────────
	if !dev.IsDFU {
		log.Printf("❌ Внутренняя ошибка: %s не в DFU.", dev.GetFriendlyName())
		m.notifier.RestoreFailed(dev, "устройство не в DFU")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	if dev.ECID == "" {
		log.Printf("❌ DFU устройство %s без ECID – прошивка невозможна.", dev.GetFriendlyName())
		m.notifier.RestoreFailed(dev, "ECID отсутствует")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	targetECID := dev.ECID

	// ── Запуск cfgutil restore ───────────────────────────
	log.Printf("⚙️ cfgutil restore → %s", targetECID)
	m.notifier.StartingRestore(dev)

	decimalECID, err := normalizeECIDForCfgutil(targetECID)
	if err != nil {
		log.Printf("❌ normalise ECID: %v", err)
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx, "cfgutil", "--ecid", decimalECID, "restore")
	out, execErr := cmd.CombinedOutput()

	if execErr != nil {
		if restoreCtx.Err() == context.DeadlineExceeded {
			m.notifier.RestoreFailed(dev, "cfgutil restore: таймаут")
		} else {
			msg := fmt.Sprintf("cfgutil restore: %v. %s",
				execErr, trim(out, 120))
			m.notifier.RestoreFailed(dev, msg)
		}
		log.Printf("❌ cfgutil restore error: %v\n%s", execErr, out)
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	log.Printf("✅ cfgutil restore завершился без ошибок для ECID %s", decimalECID)

	// ── Ждём выхода из DFU ───────────────────────────────
	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	defer waitCancel()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for exited := false; !exited; {
		select {
		case <-waitCtx.Done():
			log.Printf("⚠️ Устройство %s осталось в DFU после restore.", dev.GetFriendlyName())
			m.notifier.RestoreFailed(dev, "устройство осталось в DFU после restore")
			m.stats.DeviceCompleted(false, time.Since(start))
			return
		case <-ticker.C:
			inDFU := false
			for _, d := range m.dfuManager.GetDFUDevices(waitCtx) {
				dec, _ := normalizeECIDForCfgutil(d.ECID)
				if dec == decimalECID {
					inDFU = true
					break
				}
			}
			if !inDFU {
				exited = true
			}
		}
	}

	// ── Успех ────────────────────────────────────────────
	log.Printf("🎉 Восстановление завершено: %s", dev.GetFriendlyName())
	m.notifier.RestoreCompleted(dev)
	m.stats.DeviceCompleted(true, time.Since(start))
}

/*
   ──────────────────────────────────────────────────────────
   UTILITIES (без изменений по логике)
   ──────────────────────────────────────────────────────────
*/

func hexToDec(hexStr string) (string, error) {
	clean := strings.TrimPrefix(strings.ToLower(hexStr), "0x")
	val, err := strconv.ParseUint(clean, 16, 64)
	if err != nil {
		return "", fmt.Errorf("парсинг HEX '%s': %w", hexStr, err)
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

func normalizeECIDForCfgutil(ecid string) (string, error) {
	if ecid == "" {
		return "", fmt.Errorf("ECID пуст")
	}
	if isDigits(ecid) {
		return ecid, nil
	}
	if strings.HasPrefix(strings.ToLower(ecid), "0x") || !isDigits(ecid) {
		return hexToDec(ecid)
	}
	return "", fmt.Errorf("неизвестный формат ECID: %s", ecid)
}

func trim(b []byte, n int) string {
	s := strings.ReplaceAll(string(b), "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
