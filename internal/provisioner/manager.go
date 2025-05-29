package provisioner

import (
	"bufio"
	"bytes"
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
	dfuManager *dfu.Manager
	notifier   *notification.Manager
	stats      *stats.Manager

	processing    map[string]bool // ключ — Device.UniqueID()
	processingUSB map[string]bool // ключ — USBLocation (порт)

	processingMu sync.RWMutex
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager, stats *stats.Manager) *Manager {
	return &Manager{
		dfuManager:    dfuMgr,
		notifier:      notifier,
		stats:         stats,
		processing:    make(map[string]bool),
		processingUSB: make(map[string]bool),
	}
}

/*
──────────────────────────────────────────────────────────
        PUBLIC
──────────────────────────────────────────────────────────
*/

// IsProcessingUSB — занят ли этот USB-порт активной прошивкой
func (m *Manager) IsProcessingUSB(loc string) bool {
	if loc == "" {
		return false
	}
	m.processingMu.RLock()
	defer m.processingMu.RUnlock()
	return m.processingUSB[loc]
}

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	uid := dev.UniqueID()

	// ---- блокируем повторную обработку того же UID ----
	m.processingMu.Lock()
	if m.processing[uid] {
		m.processingMu.Unlock()
		log.Printf("ℹ️ Уже обрабатывается: %s", dev.GetFriendlyName())
		return
	}
	m.processing[uid] = true
	if dev.USBLocation != "" {
		m.processingUSB[dev.USBLocation] = true // отмечаем порт
	}
	m.processingMu.Unlock()

	// по завершении снимаем все отметки
	defer func() {
		m.processingMu.Lock()
		delete(m.processing, uid)
		if dev.USBLocation != "" {
			delete(m.processingUSB, dev.USBLocation)
		}
		m.processingMu.Unlock()
	}()

	// ---------------------------------------------------

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

	/*
	   ────────────────────────────────────────────
	   cfgutil restore
	   ────────────────────────────────────────────
	*/
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx, "cfgutil", "--ecid", decECID, "restore")

	stdOutPipe, _ := cmd.StdoutPipe()
	stdErrPipe, _ := cmd.StderrPipe()

	var stdoutBuf, stderrBuf bytes.Buffer
	stdOut := io.TeeReader(stdOutPipe, &stdoutBuf)
	stdErr := io.TeeReader(stdErrPipe, &stderrBuf)

	if err := cmd.Start(); err != nil {
		log.Printf("❌ Не удалось запустить cfgutil (%s): %v",
			strings.Join(cmd.Args, " "), err)
		m.notifier.RestoreFailed(dev, "не удалось запустить cfgutil")
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}

	progressRx := regexp.MustCompile(`(?i)(progress|percent)[:\s]+(\d{1,3})%?`)
	go m.streamCfgutilOutput(dev, stdOut, progressRx)
	go m.streamCfgutilOutput(dev, stdErr, progressRx)

	waitErr := cmd.Wait()
	if waitErr != nil {
		fullCmd := strings.Join(cmd.Args, " ")
		log.Printf(`
⚠️ cfgutil завершился с ошибкой
   Команда : %s
   Ошибка  : %v
─── STDOUT ────────────────────────────────────────────────
%s
─── STDERR ────────────────────────────────────────────────
%s
───────────────────────────────────────────────────────────`,
			fullCmd, waitErr, stdoutBuf.String(), stderrBuf.String())

		humanErr := extractRestoreError(stderrBuf.String(), waitErr)
		if restoreCtx.Err() == context.DeadlineExceeded {
			humanErr = "таймаут cfgutil restore"
		}
		m.notifier.RestoreFailed(dev, humanErr)
		m.stats.DeviceCompleted(false, time.Since(start))
		return
	}
	log.Printf("✅ cfgutil завершился для %s", dev.GetFriendlyName())

	// ждём выхода устройства из DFU
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
        Helpers — парсинг прогресса, ожидание DFU-exit и т.д.
──────────────────────────────────────────────────────────*/

// строковый прогресс cfgutil
func (m *Manager) streamCfgutilOutput(dev *device.Device, r io.Reader, rx *regexp.Regexp) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if m.parseProgressLine(dev, line, rx) {
			continue
		}

		lc := strings.ToLower(line)
		switch {
		case strings.Contains(lc, "preparing"):
			m.notifier.RestoreProgress(dev, "подготовка")
		case strings.Contains(lc, "downloading"):
			m.notifier.RestoreProgress(dev, "загрузка прошивки")
		}
	}
}

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

	Ошибка cfgutil → короткое пояснение

──────────────────────────────────────────────────────────
*/
func extractRestoreError(stderr string, waitErr error) string {
	reUSB := regexp.MustCompile(`libusbrestore\s+error[:\s]*(\d+)`)
	reCode := regexp.MustCompile(`Code[:\s]*(\d+)`)

	if m := reUSB.FindStringSubmatch(stderr); len(m) == 2 {
		return mapRestoreErrorCode(m[1])
	}
	if m := reCode.FindStringSubmatch(stderr); len(m) == 2 {
		return mapRestoreErrorCode(m[1])
	}
	if strings.Contains(stderr, "Failed to restore device in recovery mode") {
		return "ошибка восстановления (recovery mode)"
	}
	return waitErr.Error()
}

func mapRestoreErrorCode(codeStr string) string {
	switch codeStr {
	case "21":
		return "ошибка восстановления (код 21)"
	case "9":
		return "устройство неожиданно отключилось (код 9)"
	case "40":
		return "не удалось прошить (код 40)"
	case "14":
		return "архив прошивки повреждён (код 14)"
	default:
		return fmt.Sprintf("ошибка восстановления (код %s)", codeStr)
	}
}

/*
──────────────────────────────────────────────────────────

	UTILITIES

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
