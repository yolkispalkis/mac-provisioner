package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
)

type Manager struct {
	dfuManager      *dfu.Manager
	notifier        *notification.Manager
	cooldownManager *CooldownManager
	processingECID  map[string]bool
	processingUSB   map[string]bool
	mutex           sync.RWMutex
	config          config.ProvisioningConfig
	debugMode       bool
}

type RestoreResponse struct {
	Type    string `json:"Type"`
	Message string `json:"Message"`
	Code    int    `json:"Code"`
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager, cfg config.ProvisioningConfig) *Manager {
	return &Manager{
		dfuManager:      dfuMgr,
		notifier:        notifier,
		cooldownManager: NewCooldownManager(cfg.DFUCooldownPeriod),
		processingECID:  make(map[string]bool),
		processingUSB:   make(map[string]bool),
		config:          cfg,
		debugMode:       os.Getenv("MAC_PROV_DEBUG") == "1",
	}
}

func (m *Manager) IsProcessingByECID(ecid string) bool {
	if ecid == "" {
		return false
	}
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.processingECID[ecid]
}

func (m *Manager) IsProcessingByUSB(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.processingUSB[usbLocation]
}

func (m *Manager) IsDeviceInCooldown(ecid string) (bool, time.Duration, string) {
	return m.cooldownManager.IsDeviceInCooldown(ecid)
}

func (m *Manager) ShouldTriggerDFU(deviceECID string) (bool, string) {
	return m.cooldownManager.ShouldTriggerDFU(deviceECID)
}

func (m *Manager) RemoveDeviceCooldown(ecid string) {
	m.cooldownManager.RemoveCooldown(ecid)
}

func (m *Manager) MarkUSBProcessing(usbLocation string, processing bool) {
	if usbLocation == "" {
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if processing {
		m.processingUSB[usbLocation] = true
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Помечен как обрабатываемый: %s", usbLocation)
		}
	} else {
		delete(m.processingUSB, usbLocation)
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Снята отметка обработки: %s", usbLocation)
		}
	}
}

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	if !dev.IsDFU || dev.ECID == "" {
		m.notifier.RestoreFailed(dev, "устройство не готово к прошивке")
		return
	}

	m.mutex.Lock()
	if m.processingECID[dev.ECID] {
		m.mutex.Unlock()
		if m.debugMode {
			log.Printf("🔍 [DEBUG] Устройство с ECID %s уже обрабатывается", dev.ECID)
		}
		return
	}
	m.processingECID[dev.ECID] = true
	if dev.USBLocation != "" {
		m.processingUSB[dev.USBLocation] = true
	}
	m.mutex.Unlock()

	defer func() {
		m.mutex.Lock()
		delete(m.processingECID, dev.ECID)
		if dev.USBLocation != "" {
			delete(m.processingUSB, dev.USBLocation)
		}
		m.mutex.Unlock()
	}()

	ecid, err := m.normalizeECID(dev.ECID)
	if err != nil {
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	log.Printf("🔄 Начинается прошивка %s", dev.GetDisplayName())
	m.notifier.StartingRestore(dev)

	progressCtx, cancelProgress := context.WithCancel(ctx)
	go m.announceProgress(progressCtx, dev)

	err = m.runRestore(ctx, ecid)
	cancelProgress()

	if err != nil {
		log.Printf("❌ Ошибка прошивки %s: %v", dev.GetDisplayName(), err)
		m.notifier.RestoreFailed(dev, err.Error())
	} else {
		log.Printf("✅ Прошивка завершена: %s", dev.GetDisplayName())
		m.notifier.RestoreCompleted(dev)
		// Добавляем устройство в кулдаун по ECID
		m.cooldownManager.AddCompletedDevice(dev.USBLocation, dev.ECID, dev.GetDisplayName())
	}
}

func (m *Manager) runRestore(ctx context.Context, ecid string) error {
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(restoreCtx, "cfgutil", "--ecid", ecid, "--format", "JSON", "restore")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if m.debugMode {
		log.Printf("🔍 [DEBUG] Выполняем: cfgutil --ecid %s --format JSON restore", ecid)
	}

	err := cmd.Run()

	if m.debugMode {
		if stdout.Len() > 0 {
			log.Printf("🔍 [DEBUG] cfgutil stdout: %s", stdout.String())
		}
		if stderr.Len() > 0 {
			log.Printf("🔍 [DEBUG] cfgutil stderr: %s", stderr.String())
		}
	}

	if err != nil {
		if restoreCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("таймаут прошивки")
		}
		return fmt.Errorf("ошибка выполнения cfgutil: %v", err)
	}

	return m.parseRestoreResult(stdout.String())
}

func (m *Manager) parseRestoreResult(output string) error {
	lines := strings.Split(output, "\n")
	var jsonLine string

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			jsonLine = line
			break
		}
	}

	if jsonLine == "" {
		return fmt.Errorf("не найден JSON ответ от cfgutil")
	}

	var response RestoreResponse
	if err := json.Unmarshal([]byte(jsonLine), &response); err != nil {
		return fmt.Errorf("ошибка парсинга ответа cfgutil: %v", err)
	}

	switch response.Type {
	case "CommandOutput":
		return nil
	case "Error":
		return fmt.Errorf(m.mapErrorCode(response.Code, response.Message))
	default:
		return fmt.Errorf("неизвестный тип ответа: %s", response.Type)
	}
}

func (m *Manager) mapErrorCode(code int, message string) string {
	switch code {
	case 9:
		return "устройство отключилось во время прошивки"
	case 14:
		return "поврежден файл прошивки"
	case 21:
		return "ошибка восстановления системы"
	case 40:
		return "не удалось завершить прошивку"
	default:
		return fmt.Sprintf("ошибка прошивки (код %d): %s", code, message)
	}
}

func (m *Manager) normalizeECID(ecid string) (string, error) {
	if ecid == "" {
		return "", fmt.Errorf("ECID не может быть пустым")
	}

	if m.isDecimal(ecid) {
		return ecid, nil
	}

	clean := strings.TrimPrefix(strings.ToLower(ecid), "0x")
	value, err := strconv.ParseUint(clean, 16, 64)
	if err != nil {
		return "", fmt.Errorf("неверный формат ECID: %v", err)
	}

	return strconv.FormatUint(value, 10), nil
}

func (m *Manager) isDecimal(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func (m *Manager) announceProgress(ctx context.Context, dev *device.Device) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.notifier.RestoreProgress(dev, "прошивка продолжается")
		}
	}
}

func (m *Manager) GetCooldownStatus() []*CooldownEntry {
	return m.cooldownManager.GetAllCooldowns()
}
