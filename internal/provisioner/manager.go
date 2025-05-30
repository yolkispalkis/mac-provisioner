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

	// блокировка повторной обработки
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

		// Если больше нет активных прошивок – чистим кеш Configurator
		m.tryCleanupCache()
	}()

	// проверки
	if !dev.IsDFU || dev.ECID == "" {
		m.notif.RestoreFailed(dev, "устройство не в DFU или нет ECID")
		return
	}
	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notif.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	// звук
	m.voice.MelodyOn()
	defer m.voice.MelodyOff()
	m.notif.StartingRestore(dev)

	annDone := make(chan struct{})
	go m.announceLoop(ctx, annDone, dev)

	// cfgutil restore
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx,
		"cfgutil", "--ecid", decECID, "--format", "JSON", "restore")

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	close(annDone)

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

const configuratorTmpRel = "Library/Containers/com.apple.configurator.xpc.DeviceService/Data/tmp"

// tryCleanupCache запускает очистку кеша, если нет активных прошивок.
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
			log.Printf("⚠️ Очистка кеша Apple Configurator: %v", err)
		} else {
			log.Print("🧹 Кеш Apple Configurator очищен")
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
			return nil // директории нет – нечего чистить
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

	// 1) делим по «/»: первая часть — root-порт, остальные — хабы
	parts := strings.Split(loc, "/")
	rootRaw := strings.TrimSpace(parts[0])

	// — корневой порт
	rootStr := "неизвестный"
	if m := rxRoot.FindStringSubmatch(rootRaw); len(m) == 3 {
		if n, err := strconv.ParseInt(m[2], 16, 0); err == nil {
			rootStr = fmt.Sprintf("порт %d", n)
		}
	}

	// — цепочка хабов, если есть
	if len(parts) == 1 {
		return rootStr
	}
	hubs := strings.Join(parts[1:], "-")
	return fmt.Sprintf("хаб %s, порт %d", hubs, rootStr)
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
