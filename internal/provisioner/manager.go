package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
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
)

/*
──────────────────────────────────────────────────────────

	STRUCT

──────────────────────────────────────────────────────────
*/
type Manager struct {
	dfuManager *dfu.Manager
	notifier   *notification.Manager

	processing    map[string]bool // ключ — Device.UniqueID()
	processingUSB map[string]bool // ключ — USBLocation (порт)

	processingMu sync.RWMutex
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager) *Manager {
	return &Manager{
		dfuManager:    dfuMgr,
		notifier:      notifier,
		processing:    make(map[string]bool),
		processingUSB: make(map[string]bool),
	}
}

/*
──────────────────────────────────────────────────────────
        PUBLIC
──────────────────────────────────────────────────────────
*/

// IsProcessingUSB — занят ли этот USB-порт активной прошивкой?
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

	// по завершении снимаем отметки
	defer func() {
		m.processingMu.Lock()
		delete(m.processing, uid)
		if dev.USBLocation != "" {
			delete(m.processingUSB, dev.USBLocation)
		}
		m.processingMu.Unlock()
	}()

	//----------------------------------------------------

	log.Printf("🚀 Старт прошивки: %s (ECID:%s)", dev.GetFriendlyName(), dev.ECID)

	if !dev.IsDFU || dev.ECID == "" {
		errMsg := "устройство не готово к прошивке (нет DFU или ECID)"
		log.Printf("❌ %s: %s", dev.GetFriendlyName(), errMsg)
		m.notifier.RestoreFailed(dev, errMsg)
		return
	}

	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	m.notifier.StartingRestore(dev)

	/*
	   ────────────────────────────────────────────
	   cfgutil --format JSON restore
	   ────────────────────────────────────────────
	*/
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx,
		"cfgutil",
		"--ecid", decECID,
		"--format", "JSON",
		"restore",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// команда не запустилась или упала вне собственного отчёта
		log.Printf("⚠️ cfgutil error: %v, stderr: %s", err, stderr.String())
		human := "не удалось запустить cfgutil"
		if restoreCtx.Err() == context.DeadlineExceeded {
			human = "таймаут cfgutil restore"
		}
		m.notifier.RestoreFailed(dev, human)
		return
	}

	// cfgutil всегда пишет ровно одну JSON-строку
	line := strings.TrimSpace(stdout.String())
	if line == "" {
		m.notifier.RestoreFailed(dev, "пустой ответ cfgutil")
		return
	}

	var resp cfgutilJSON
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		log.Printf("⚠️ Некорректный JSON от cfgutil: %v\n%s", err, line)
		m.notifier.RestoreFailed(dev, "некорректный ответ cfgutil")
		return
	}

	// --- анализ результата ---
	switch resp.Type {
	case "CommandOutput":
		log.Printf("🎉 Прошивка завершена: %s", dev.GetFriendlyName())
		m.notifier.RestoreCompleted(dev)

	case "Error":
		humanErr := mapRestoreErrorCode(strconv.Itoa(resp.Code))
		if humanErr == "" { // если кода нет в мапе — короткое сообщение
			humanErr = resp.Message
		}
		m.notifier.RestoreFailed(dev, humanErr)
		log.Printf("❌ cfgutil Error (%d): %s", resp.Code, resp.Message)

	default:
		m.notifier.RestoreFailed(dev, "неизвестный ответ cfgutil")
		log.Printf("⚠️ Неизвестный Type %q в JSON cfgutil", resp.Type)
	}
}

/*
──────────────────────────────────────────────────────────

	JSON-ответ cfgutil

──────────────────────────────────────────────────────────
*/
type cfgutilJSON struct {
	// Для Error-ветки
	Domain  string `json:"Domain,omitempty"`
	Message string `json:"Message,omitempty"`
	Code    int    `json:"Code,omitempty"`
	Type    string `json:"Type"`

	// Для успешного завершения
	Command string   `json:"Command,omitempty"`
	Devices []string `json:"Devices,omitempty"`
}

/*
──────────────────────────────────────────────────────────

	Ошибка cfgutil → короткое пояснение

──────────────────────────────────────────────────────────
*/
func mapRestoreErrorCode(codeStr string) string {
	// встречаются коды 9, 14, 21, 40 …
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
		return ""
	}
}

/*
──────────────────────────────────────────────────────────

	UTILITIES (ECID helpers)

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
