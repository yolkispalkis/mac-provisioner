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

type Manager struct {
	dfuManager *dfu.Manager
	notifier   *notification.Manager
	processing map[string]bool
	mutex      sync.RWMutex
}

type RestoreResponse struct {
	Type    string `json:"Type"`
	Message string `json:"Message"`
	Code    int    `json:"Code"`
}

func New(dfuMgr *dfu.Manager, notifier *notification.Manager) *Manager {
	return &Manager{
		dfuManager: dfuMgr,
		notifier:   notifier,
		processing: make(map[string]bool),
	}
}

func (m *Manager) IsProcessing(deviceID string) bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.processing[deviceID]
}

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	deviceID := dev.UniqueID()

	// Проверяем, не обрабатывается ли уже это устройство
	m.mutex.Lock()
	if m.processing[deviceID] {
		m.mutex.Unlock()
		log.Printf("ℹ️ Устройство %s уже обрабатывается", dev.Name)
		return
	}
	m.processing[deviceID] = true
	m.mutex.Unlock()

	defer func() {
		m.mutex.Lock()
		delete(m.processing, deviceID)
		m.mutex.Unlock()
	}()

	// Проверяем готовность устройства
	if !dev.IsDFU || dev.ECID == "" {
		m.notifier.RestoreFailed(dev, "устройство не готово к прошивке")
		return
	}

	// Нормализуем ECID для cfgutil
	ecid, err := m.normalizeECID(dev.ECID)
	if err != nil {
		m.notifier.RestoreFailed(dev, "неверный формат ECID")
		return
	}

	log.Printf("🔄 Начинаем прошивку устройства %s (ECID: %s)", dev.Name, ecid)
	m.notifier.StartingRestore(dev)

	// Запускаем периодические уведомления о прогрессе
	progressCtx, cancelProgress := context.WithCancel(ctx)
	go m.announceProgress(progressCtx, dev)

	// Выполняем прошивку
	err = m.runRestore(ctx, ecid)
	cancelProgress()

	if err != nil {
		m.notifier.RestoreFailed(dev, err.Error())
	} else {
		m.notifier.RestoreCompleted(dev)
	}
}

func (m *Manager) runRestore(ctx context.Context, ecid string) error {
	// Создаем контекст с таймаутом
	restoreCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	// Запускаем cfgutil restore
	cmd := exec.CommandContext(restoreCtx, "cfgutil", "--ecid", ecid, "--format", "JSON", "restore")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Анализируем результат
	if err != nil {
		if restoreCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("таймаут прошивки")
		}
		return fmt.Errorf("ошибка выполнения cfgutil: %v", err)
	}

	// Парсим JSON ответ
	return m.parseRestoreResult(stdout.String())
}

func (m *Manager) parseRestoreResult(output string) error {
	// Ищем последнюю JSON строку в выводе
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
		return nil // Успех
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

	// Если это уже десятичное число
	if m.isDecimal(ecid) {
		return ecid, nil
	}

	// Конвертируем из hex в decimal
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
