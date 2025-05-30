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
	"syscall"
	"time"

	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
)

/*─────────────────────────────────────────────────────────
                         melodyPlayer
─────────────────────────────────────────────────────────*/

type melodyPlayer struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	ctx  context.Context
	stop context.CancelFunc
}

func newMelodyPlayer(parent context.Context, file string) *melodyPlayer {
	ctx, cancel := context.WithCancel(parent)
	p := &melodyPlayer{ctx: ctx, stop: cancel}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				cmd := exec.CommandContext(ctx, "afplay", file)
				p.mu.Lock()
				p.cmd = cmd
				p.mu.Unlock()
				_ = cmd.Run() // когда файл закончится – перезапустим
			}
		}
	}()
	return p
}

func (p *melodyPlayer) Pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGSTOP)
	}
}

func (p *melodyPlayer) Resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGCONT)
	}
}

func (p *melodyPlayer) Stop() { p.stop() }

/*─────────────────────────────────────────────────────────
                          Manager
─────────────────────────────────────────────────────────*/

type Manager struct {
	dfuManager *dfu.Manager
	notifier   *notification.Manager

	processing    map[string]bool // Device.UniqueID()
	processingUSB map[string]bool // USB-порт
	processingMu  sync.RWMutex
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager) *Manager {
	return &Manager{
		dfuManager:    dfuMgr,
		notifier:      notifier,
		processing:    make(map[string]bool),
		processingUSB: make(map[string]bool),
	}
}

/*─────────────────────────────────────────────────────────
                       PUBLIC API
─────────────────────────────────────────────────────────*/

func (m *Manager) IsProcessingUSB(loc string) bool {
	m.processingMu.RLock()
	defer m.processingMu.RUnlock()
	return loc != "" && m.processingUSB[loc]
}

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	uid := dev.UniqueID()

	// блокируем повторную обработку
	m.processingMu.Lock()
	if m.processing[uid] {
		m.processingMu.Unlock()
		log.Printf("ℹ️ Уже обрабатывается: %s", dev.GetFriendlyName())
		return
	}
	m.processing[uid] = true
	if dev.USBLocation != "" {
		m.processingUSB[dev.USBLocation] = true
	}
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, uid)
		if dev.USBLocation != "" {
			delete(m.processingUSB, dev.USBLocation)
		}
		m.processingMu.Unlock()
	}()

	// ───────────────────────────────────────────────

	if !dev.IsDFU || dev.ECID == "" {
		m.notifier.RestoreFailed(dev, "устройство не в DFU или нет ECID")
		return
	}

	decECID, err := normalizeECIDForCfgutil(dev.ECID)
	if err != nil {
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	// фоновая мелодия + периодические объявления
	player := newMelodyPlayer(ctx, "/System/Library/Sounds/Submarine.aiff")
	defer player.Stop()

	m.speakWithMelody(player, func() { m.notifier.StartingRestore(dev) })

	auxDone := make(chan struct{})
	go m.announceLoop(ctx, auxDone, player, dev)

	// ───────────────────────────────────────────────
	// cfgutil --format JSON restore
	// ───────────────────────────────────────────────
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx,
		"cfgutil", "--ecid", decECID, "--format", "JSON", "restore",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	close(auxDone) // стоп анонсов

	if runErr != nil {
		m.speakWithMelody(player, func() {
			msg := "не удалось запустить cfgutil"
			if restoreCtx.Err() == context.DeadlineExceeded {
				msg = "таймаут cfgutil restore"
			}
			m.notifier.RestoreFailed(dev, msg)
		})
		return
	}

	line := strings.TrimSpace(stdout.String())
	var resp cfgutilJSON
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		m.speakWithMelody(player, func() {
			m.notifier.RestoreFailed(dev, "некорректный ответ cfgutil")
		})
		return
	}

	switch resp.Type {
	case "CommandOutput":
		m.speakWithMelody(player, func() { m.notifier.RestoreCompleted(dev) })

	case "Error":
		human := mapRestoreErrorCode(strconv.Itoa(resp.Code))
		if human == "" {
			human = resp.Message
		}
		m.speakWithMelody(player, func() { m.notifier.RestoreFailed(dev, human) })

	default:
		m.speakWithMelody(player, func() { m.notifier.RestoreFailed(dev, "неизвестный ответ cfgutil") })
	}
}

/*─────────────────────────────────────────────────────────
                 Melody ↔ Speech кооперация
─────────────────────────────────────────────────────────*/

func (m *Manager) speakWithMelody(mp *melodyPlayer, speakFn func()) {
	mp.Pause()
	speakFn()
	for m.notifier.IsPlaying() {
		time.Sleep(150 * time.Millisecond)
	}
	mp.Resume()
}

func (m *Manager) announceLoop(ctx context.Context, done <-chan struct{},
	mp *melodyPlayer, dev *device.Device) {

	t := time.NewTicker(15 * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			port := strings.TrimPrefix(dev.USBLocation, "0x")
			msg := fmt.Sprintf("идёт прошивка %s, порт %s",
				dev.GetReadableModel(), port)
			m.speakWithMelody(mp, func() { m.notifier.RestoreProgress(dev, msg) })
		}
	}
}

/*─────────────────────────────────────────────────────────
                   cfgutil JSON ответ
─────────────────────────────────────────────────────────*/

type cfgutilJSON struct {
	Type    string `json:"Type"`
	Message string `json:"Message,omitempty"`
	Code    int    `json:"Code,omitempty"`

	Command string   `json:"Command,omitempty"`
	Devices []string `json:"Devices,omitempty"`
}

/*─────────────────────────────────────────────────────────
             mapRestoreErrorCode (как раньше)
─────────────────────────────────────────────────────────*/

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
		return ""
	}
}

/*─────────────────────────────────────────────────────────
             ECID helpers (без изменений)
─────────────────────────────────────────────────────────*/

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
