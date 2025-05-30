package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

	processing    map[string]bool
	processingUSB map[string]bool
	mu            sync.RWMutex

	cleaningCache bool // true – идёт асинхронная очистка кеша
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

	// ── блокировка повторной обработки ────────────────────
	m.mu.Lock()
	if m.processing[uid] {
		m.mu.Unlock()
		log.Printf("ℹ️  Уже обрабатывается: %s", dev.GetFriendlyName())
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

		// Если больше нет активных прошивок – чистим кеш Configurator
		m.tryCleanupCache()
	}()

	// ── проверки ───────────────────────────────────────────
	if !dev.IsDFU || dev.ECID == "" {
		m.notif.RestoreFailed(dev, "устройство не в DFU или нет ECID")
		return
	}
	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notif.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	// ── звук / уведомления ────────────────────────────────
	m.voice.MelodyOn()
	defer m.voice.MelodyOff()
	m.notif.StartingRestore(dev)

	annDone := make(chan struct{})
	go m.announceLoop(ctx, annDone, dev)

	// ── cfgutil restore ───────────────────────────────────
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(
		restoreCtx,
		"cfgutil", "--ecid", decECID, "--format", "JSON", "restore",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	close(annDone)

	// --- 1. пытаемся извлечь JSON-ответ -------------------
	jsonLine := lastJSONLine(stdout.String())

	if jsonLine == "" {
		// JSON нет → обобщённая ошибка/таймаут
		msg := "cfgutil завершился с ошибкой"
		if restoreCtx.Err() == context.DeadlineExceeded {
			msg = "таймаут cfgutil restore"
		}
		if runErr != nil {
			log.Printf("⚠️  cfgutil error: %v — %s", runErr, stderr.String())
		}
		m.notif.RestoreFailed(dev, msg)
		return
	}

	var resp cfgutilJSON
	if err := json.Unmarshal([]byte(jsonLine), &resp); err != nil {
		log.Printf("⚠️  bad cfgutil JSON: %v\n%s", err, jsonLine)
		m.notif.RestoreFailed(dev, "некорректный ответ cfgutil")
		return
	}

	// --- 2. интерпретируем JSON ---------------------------
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
	}
}

/*
──────────────────────────────────────────────────────────
        Periodic “in-progress” announcements
──────────────────────────────────────────────────────────
*/

func (m *Manager) announceLoop(ctx context.Context, done <-chan struct{}, dev *device.Device) {
	t := time.NewTicker(60 * time.Second)
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
        Apple Configurator cache cleanup
──────────────────────────────────────────────────────────
*/

const configuratorTmpRel = "Library/Containers/com.apple.configurator.xpc.DeviceService/Data/tmp/TemporaryItems"

func (m *Manager) tryCleanupCache() {
	m.mu.Lock()
	if len(m.processing) > 0 || m.cleaningCache {
		m.mu.Unlock()
		return
	}
	m.cleaningCache = true
	m.mu.Unlock()

	go func() {
		if err := m.cleanConfiguratorCache(); err != nil {
			log.Printf("⚠️  Очистка кеша Apple Configurator: %v", err)
		} else {
			log.Print("🧹  Кеш Apple Configurator очищен")
		}
		m.mu.Lock()
		m.cleaningCache = false
		m.mu.Unlock()
	}()
}

func (m *Manager) cleanConfiguratorCache() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}

	cacheDir := filepath.Join(home, configuratorTmpRel)

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(cacheDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

/*
──────────────────────────────────────────────────────────
        PORT helper 0x00100000/1 → «порт 1, хаб 1»
──────────────────────────────────────────────────────────
*/

var rxRoot = regexp.MustCompile(`(?i)^(0x)?00([0-9a-f])0000`)

func humanPort(loc string) string {
	if loc == "" {
		return "неизвестный порт"
	}

	base := strings.Split(loc, "/")[0]
	base = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(base), "0x"))
	if base == "" {
		return "неизвестный порт"
	}

	switch {
	case len(base) < 8:
		base = strings.Repeat("0", 8-len(base)) + base
	case len(base) > 8:
		base = base[len(base)-8:]
	}

	var ports []int
	for i := 0; i < len(base); i++ {
		v, err := strconv.ParseInt(base[i:i+1], 16, 0)
		if err != nil {
			return "неизвестный порт"
		}
		if v != 0 {
			ports = append(ports, int(v))
		}
	}
	if len(ports) == 0 {
		return "неизвестный порт"
	}

	root := ports[0]
	if len(ports) == 1 {
		return fmt.Sprintf("порт %d", root)
	}

	hubs := make([]string, len(ports)-1)
	for i, p := range ports[1:] {
		hubs[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("хаб %s, порт %d", strings.Join(hubs, "-"), root)
}

/*
──────────────────────────────────────────────────────────
        cfgutil JSON → struct
──────────────────────────────────────────────────────────
*/

type cfgutilJSON struct {
	Type    string `json:"Type"`
	Message string `json:"Message,omitempty"`
	Code    int    `json:"Code,omitempty"`
}

/*
──────────────────────────────────────────────────────────
        mapRestoreErrorCode
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

/*
──────────────────────────────────────────────────────────
        helper: достаём последнюю строку, похожую на JSON
──────────────────────────────────────────────────────────
*/

func lastJSONLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}
