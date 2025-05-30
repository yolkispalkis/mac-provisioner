package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"mac-provisioner/internal/voice"
)

/*
──────────────────────────────────────────────────────────

	STRUCT

──────────────────────────────────────────────────────────
*/
type Manager struct {
	dfuMgr *dfu.Manager
	notif  *notification.Manager
	voice  *voice.Engine

	processing    map[string]bool // UID  → true
	processingUSB map[string]bool // USB → true
	mu            sync.RWMutex
}

func New(dfuMgr *dfu.Manager, n *notification.Manager, v *voice.Engine) *Manager {
	return &Manager{
		dfuMgr:        dfuMgr,
		notif:         n,
		voice:         v,
		processing:    map[string]bool{},
		processingUSB: map[string]bool{},
	}
}

/*
──────────────────────────────────────────────────────────
                               PUBLIC
──────────────────────────────────────────────────────────
*/

// IsProcessingUSB — идёт ли прошивка на данном USB-порту
func (m *Manager) IsProcessingUSB(port string) bool {
	if port == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.processingUSB[port]
}

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	uid := dev.UniqueID()

	//----------------------------------------------------
	// 1) блокировка повторной обработки UID / USB-порта
	//----------------------------------------------------
	m.mu.Lock()
	if m.processing[uid] {
		m.mu.Unlock()
		log.Printf("ℹ️ Уже обрабатывается: %s", dev.GetFriendlyName())
		return
	}
	m.processing[uid] = true
	if dev.USBLocation != "" {
		m.processingUSB[dev.USBLocation] = true
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.processing, uid)
		if dev.USBLocation != "" {
			delete(m.processingUSB, dev.USBLocation)
		}
		m.mu.Unlock()
	}()

	//----------------------------------------------------
	// 2) проверки
	//----------------------------------------------------
	if !dev.IsDFU || dev.ECID == "" {
		m.notif.RestoreFailed(dev, "устройство не в DFU или нет ECID")
		return
	}
	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notif.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	//----------------------------------------------------
	// 3) звук: фон + «старт прошивки»
	//----------------------------------------------------
	m.voice.MelodyOn()
	defer m.voice.MelodyOff()

	m.notif.StartingRestore(dev)

	// периодический анонс «идёт прошивка, порт …»
	annDone := make(chan struct{})
	go m.announceLoop(ctx, annDone, dev)

	//----------------------------------------------------
	// 4) cfgutil --format JSON restore
	//----------------------------------------------------
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx,
		"cfgutil", "--ecid", decECID, "--format", "JSON", "restore")

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	close(annDone) // останавливаем анонсы

	if runErr != nil {
		msg := "не удалось запустить cfgutil"
		if restoreCtx.Err() == context.DeadlineExceeded {
			msg = "таймаут cfgutil restore"
		}
		log.Printf("⚠️ cfgutil error: %v — %s", runErr, stderr.String())
		m.notif.RestoreFailed(dev, msg)
		return
	}

	line := strings.TrimSpace(stdout.String())
	if line == "" {
		m.notif.RestoreFailed(dev, "пустой ответ cfgutil")
		return
	}

	var resp cfgutilJSON
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		log.Printf("⚠️ bad cfgutil JSON: %v\n%s", err, line)
		m.notif.RestoreFailed(dev, "некорректный ответ cfgutil")
		return
	}

	switch resp.Type {
	case "CommandOutput":
		log.Printf("🎉 Прошивка успешна: %s", dev.GetFriendlyName())
		m.notif.RestoreCompleted(dev)

	case "Error":
		human := mapRestoreErrorCode(strconv.Itoa(resp.Code))
		if human == "" {
			human = resp.Message
		}
		m.notif.RestoreFailed(dev, human)
		log.Printf("❌ cfgutil Error (%d): %s", resp.Code, resp.Message)

	default:
		m.notif.RestoreFailed(dev, "неизвестный ответ cfgutil")
		log.Printf("⚠️ неизвестный JSON-Type %q", resp.Type)
	}
}

/*
──────────────────────────────────────────────────────────

	Periodic “in-progress” announcements

──────────────────────────────────────────────────────────
*/
func (m *Manager) announceLoop(ctx context.Context, done <-chan struct{}, dev *device.Device) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			m.notif.RestoreProgress(dev,
				fmt.Sprintf("идёт прошивка, %s", humanPort(dev.USBLocation)))
		}
	}
}

/*
──────────────────────────────────────────────────────────

	PORT helper  0x00100000 → «порт 1»

──────────────────────────────────────────────────────────
*/
var rxPort = regexp.MustCompile(`(?i)^0x00([0-9a-f])0000$`)

func humanPort(usbLoc string) string {
	if m := rxPort.FindStringSubmatch(usbLoc); len(m) == 2 {
		hexDigit := m[1] // 1,2,…
		n, _ := strconv.ParseInt(hexDigit, 16, 0)
		return fmt.Sprintf("порт %d", n)
	}
	return "неизвестный порт"
}

/*
──────────────────────────────────────────────────────────

	cfgutil JSON → struct

──────────────────────────────────────────────────────────
*/
type cfgutilJSON struct {
	Type    string `json:"Type"`              // "CommandOutput" | "Error"
	Message string `json:"Message,omitempty"` // при ошибке
	Code    int    `json:"Code,omitempty"`
}

/*
──────────────────────────────────────────────────────────

	Error-code → человекочитаемый текст

──────────────────────────────────────────────────────────
*/
func mapRestoreErrorCode(code string) string {
	switch code {
	case "21":
		return "ошибка восстановления (код 21)"
	case "9":
		return "устройство отключилось (код 9)"
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

	ECID helpers

──────────────────────────────────────────────────────────
*/
func hexToDec(hexStr string) (string, error) {
	clean := strings.TrimPrefix(strings.ToLower(hexStr), "0x")
	v, err := strconv.ParseUint(clean, 16, 64)
	if err != nil {
		return "", fmt.Errorf("hex→dec: %w", err)
	}
	return strconv.FormatUint(v, 10), nil
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
	return hexToDec(ecid)
}
