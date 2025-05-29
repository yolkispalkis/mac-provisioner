package provisioner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"regexp"
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

	PUBLIC
	──────────────────────────────────────────────────────────
*/
func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	uid := dev.UniqueID()

	// блокируем повтор
	m.processingMu.Lock()
	if m.processing[uid] {
		m.processingMu.Unlock()
		log.Printf("ℹ️ Уже обрабатывается: %s", dev.GetFriendlyName())
		return
	}
	m.processing[uid] = true
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, uid)
		m.processingMu.Unlock()
	}()

	log.Printf("🚀 Старт прошивки: %s (ECID:%s)", dev.GetFriendlyName(), dev.ECID)
	start := time.Now()
	m.stats.DeviceStarted()

	if !dev.IsDFU || dev.ECID == "" {
		errMsg := "устройство не готово к прошивке (нет DFU или ECID)"
		log.Printf("❌ %s: %s", dev.GetFriendlyName(), errMsg)
		m.notifier.RestoreFailed(dev, errMsg)
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	m.notifier.StartingRestore(dev)

	// ──────────────────────────────────────────────────
	// cfgutil restore  (онлайн-парсинг вывода)
	// ──────────────────────────────────────────────────
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx, "cfgutil", "--ecid", decECID, "restore")
	stdOut, _ := cmd.StdoutPipe()
	stdErr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		log.Printf("❌ Не удалось запустить cfgutil: %v", err)
		m.notifier.RestoreFailed(dev, "не удалось запустить cfgutil")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	// читаем вывод
	progressRx := regexp.MustCompile(`(?i)(progress|percent)[:\s]+(\d{1,3})%?`)
	go m.streamCfgutilOutput(dev, stdOut, progressRx)
	go m.streamCfgutilOutput(dev, stdErr, progressRx) // stderr иногда содержит то же самое

	// ждём завершения
	waitErr := cmd.Wait()
	if waitErr != nil {
		if restoreCtx.Err() == context.DeadlineExceeded {
			m.notifier.RestoreFailed(dev, "таймаут cfgutil restore")
		} else {
			m.notifier.RestoreFailed(dev, waitErr.Error())
		}
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	log.Printf("✅ cfgutil завершился для %s", dev.GetFriendlyName())

	// ──────────────────────────────────────────────────
	// ждём выхода из DFU
	// ──────────────────────────────────────────────────
	if !m.waitExitDFU(ctx, decECID, 30*time.Second) {
		m.notifier.RestoreFailed(dev, "устройство осталось в DFU после restore")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	log.Printf("🎉 Прошивка завершена: %s", dev.GetFriendlyName())
	m.notifier.RestoreCompleted(dev)
	m.stats.DeviceCompleted(true, time.Since(start))
}

/*──────────────────────────────────────────────────────────
  Helpers
  ──────────────────────────────────────────────────────────*/

// читает вывод cfgutil построчно, даёт голосовые уведомления о прогрессе
func (m *Manager) streamCfgutilOutput(dev *device.Device, r io.Reader, rx *regexp.Regexp) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if m.parseProgressLine(dev, line, rx) {
			continue // прогресс уже обработан
		}

		// можно добавить дополнительные статусы
		lc := strings.ToLower(line)
		switch {
		case strings.Contains(lc, "preparing"):
			m.notifier.RestoreProgress(dev, "подготовка")
		case strings.Contains(lc, "downloading"):
			m.notifier.RestoreProgress(dev, "загрузка прошивки")
		}
	}
}

// возвращает true, если строка содержала percent
func (m *Manager) parseProgressLine(dev *device.Device, line string, rx *regexp.Regexp) bool {
	if !rx.MatchString(line) {
		return false
	}
	matches := rx.FindStringSubmatch(line)
	if len(matches) < 3 {
		return false
	}
	percent := matches[2]
	m.notifier.RestoreProgress(dev, percent+" %")
	return true
}

// ждём, пока устройство выйдет из DFU
func (m *Manager) waitExitDFU(ctx context.Context, decimalECID string, max time.Duration) bool {
	waitCtx, cancel := context.WithTimeout(ctx, max)
	defer cancel()

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return false
		case <-tick.C:
			inDFU := false
			for _, d := range m.dfuManager.GetDFUDevices(waitCtx) {
				dec, _ := normalizeECIDForCfgutil(d.ECID)
				if dec == decimalECID {
					inDFU = true
					break
				}
			}
			if !inDFU {
				return true
			}
		}
	}
}

/*
──────────────────────────────────────────────────────────

	UTILITIES (без изменений)
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
