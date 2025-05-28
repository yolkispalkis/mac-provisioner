package provisioner

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/device"
	"mac-provisioner/internal/dfu"
	"mac-provisioner/internal/notification"
	"mac-provisioner/internal/stats"
)

type Manager struct {
	dfuManager   *dfu.Manager
	notifier     *notification.Manager
	stats        *stats.Manager
	processing   map[string]bool
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

func (m *Manager) ProcessDevice(ctx context.Context, dev *device.Device) {
	// Проверяем, не обрабатывается ли уже это устройство
	m.processingMu.Lock()
	if m.processing[dev.SerialNumber] {
		m.processingMu.Unlock()
		return
	}
	m.processing[dev.SerialNumber] = true
	m.processingMu.Unlock()

	defer func() {
		m.processingMu.Lock()
		delete(m.processing, dev.SerialNumber)
		m.processingMu.Unlock()
	}()

	log.Printf("🔄 Начинается обработка устройства %s (%s)", dev.SerialNumber, dev.Model)
	startTime := time.Now()
	m.stats.DeviceStarted()

	var targetIdentifier string = dev.SerialNumber

	// Если устройство не в DFU режиме, переводим его
	if !dev.IsDFU {
		m.notifier.EnteringDFUMode(dev.SerialNumber)
		log.Printf("📱 Попытка перевода в DFU режим для устройства %s", dev.SerialNumber)

		if err := m.dfuManager.EnterDFUMode(dev.SerialNumber); err != nil {
			log.Printf("❌ Ошибка перехода в DFU режим для %s: %v", dev.SerialNumber, err)

			if strings.Contains(err.Error(), "ручного входа в DFU режим") {
				m.notifier.ManualDFURequired(dev.SerialNumber)
				log.Printf("\n" + strings.Repeat("=", 80))
				log.Printf("ТРЕБУЕТСЯ РУЧНОЙ ПЕРЕХОД В DFU РЕЖИМ")
				log.Printf(strings.Repeat("=", 80))
				log.Printf("%v", err)
				log.Printf(strings.Repeat("=", 80) + "\n")

				m.notifier.WaitingForDFU(dev.SerialNumber)
				log.Printf("⏳ Ожидание 60 секунд для ручного перехода в DFU режим...")

				// Ждем и периодически проверяем DFU устройства
				if ecid := m.waitForManualDFU(ctx, 60); ecid != "" {
					targetIdentifier = ecid
					log.Printf("🔄 Используется ECID для восстановления: %s", targetIdentifier)
				} else {
					m.notifier.RestoreFailed(dev.SerialNumber, "Устройство не перешло в режим восстановления")
					m.stats.DeviceCompleted(false, time.Since(startTime))
					return
				}
			} else {
				m.notifier.RestoreFailed(dev.SerialNumber, "Не удалось войти в режим восстановления")
				m.notifier.PlayAlert()
				m.stats.DeviceCompleted(false, time.Since(startTime))
				return
			}
		} else {
			m.notifier.DFUModeEntered(dev.SerialNumber)
			time.Sleep(5 * time.Second) // Даем время для стабилизации DFU режима

			// После входа в DFU режим получаем ECID
			if ecid := m.dfuManager.GetFirstDFUECID(); ecid != "" {
				targetIdentifier = ecid
				log.Printf("🔄 Устройство вошло в DFU режим, используется ECID: %s", targetIdentifier)
			}
		}
	} else {
		// Устройство уже в DFU режиме, используем его ECID
		if dev.ECID != "" {
			targetIdentifier = dev.ECID
			log.Printf("🔄 Устройство уже в DFU режиме, используется ECID: %s", targetIdentifier)
		}
	}

	// Начинаем восстановление
	m.notifier.StartingRestore(dev.SerialNumber)
	log.Printf("🔧 Начинается восстановление для устройства %s (идентификатор: %s)", dev.SerialNumber, targetIdentifier)

	if err := m.restoreDevice(ctx, targetIdentifier, dev.SerialNumber); err != nil {
		log.Printf("❌ Ошибка восстановления устройства %s: %v", targetIdentifier, err)
		m.notifier.RestoreFailed(dev.SerialNumber, err.Error())
		m.notifier.PlayAlert()
		m.stats.DeviceCompleted(false, time.Since(startTime))
		return
	}

	m.notifier.RestoreCompleted(dev.SerialNumber)
	m.notifier.PlaySuccess()
	m.stats.DeviceCompleted(true, time.Since(startTime))
	log.Printf("✅ Успешно восстановлено устройство %s за %v", dev.SerialNumber, time.Since(startTime).Round(time.Second))
}

func (m *Manager) waitForManualDFU(ctx context.Context, timeoutSeconds int) string {
	for i := 0; i < timeoutSeconds/2; i++ {
		select {
		case <-ctx.Done():
			return ""
		default:
		}

		time.Sleep(2 * time.Second)

		// Проверяем, есть ли DFU устройства
		dfuDevices := m.dfuManager.GetDFUDevices()
		if len(dfuDevices) > 0 {
			log.Printf("✅ Найдены DFU устройства: %+v", dfuDevices)
			return dfuDevices[0].ECID
		}

		if i%5 == 0 {
			log.Printf("⏳ Все еще ожидается DFU режим... (%d/%d секунд)", i*2, timeoutSeconds)
		}
	}

	return ""
}

func (m *Manager) restoreDevice(ctx context.Context, identifier, originalSerial string) error {
	log.Printf("Начинается восстановление для устройства %s...", identifier)

	// Определяем, это ECID или серийный номер
	var cmd *exec.Cmd
	if strings.HasPrefix(identifier, "0x") {
		// Это ECID, используем его для восстановления
		log.Printf("Используется ECID для восстановления: %s", identifier)
		cmd = exec.Command("cfgutil", "restore", "-e", identifier, "--erase")
	} else {
		// Это серийный номер
		log.Printf("Используется серийный номер для восстановления: %s", identifier)
		cmd = exec.Command("cfgutil", "restore", "-s", identifier, "--erase")
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ошибка cfgutil restore: %w", err)
	}

	return m.waitForRestoreCompletion(ctx, identifier, originalSerial)
}

func (m *Manager) waitForRestoreCompletion(ctx context.Context, identifier, originalSerial string) error {
	log.Printf("Ожидание завершения восстановления для устройства %s...", identifier)

	maxWaitTime := 30 * time.Minute
	checkInterval := 30 * time.Second

	timeout := time.After(maxWaitTime)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastStatus := ""

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("операция отменена")
		case <-timeout:
			return fmt.Errorf("таймаут восстановления для устройства %s", identifier)
		case <-ticker.C:
			status, err := m.getDeviceStatus(identifier)
			if err != nil {
				continue
			}

			log.Printf("Статус устройства %s: %s", identifier, status)

			if status != lastStatus && status != "Устройство не найдено" {
				readableStatus := m.getReadableStatus(status)
				m.notifier.RestoreProgress(originalSerial, readableStatus)
				lastStatus = status
			}
			if m.isRestoreComplete(status) {
				log.Printf("Восстановление завершено для устройства %s", identifier)
				return nil
			}
		}
	}
}

func (m *Manager) getDeviceStatus(identifier string) (string, error) {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// Ищем по ECID или серийному номеру
		if strings.Contains(line, identifier) {
			return line, nil
		}
	}

	return "Устройство не найдено", nil
}

func (m *Manager) getReadableStatus(status string) string {
	status = strings.ToLower(status)

	if strings.Contains(status, "dfu") {
		return "в режиме восстановления"
	}
	if strings.Contains(status, "recovery") {
		return "в режиме восстановления"
	}
	if strings.Contains(status, "restoring") {
		return "восстанавливает прошивку"
	}
	if strings.Contains(status, "available") {
		return "доступно"
	}
	if strings.Contains(status, "paired") {
		return "сопряжено и готово"
	}

	return "неизвестный статус"
}

func (m *Manager) isRestoreComplete(status string) bool {
	status = strings.ToLower(status)

	return !strings.Contains(status, "dfu") &&
		!strings.Contains(status, "recovery") &&
		!strings.Contains(status, "restoring") &&
		(strings.Contains(status, "available") || strings.Contains(status, "paired"))
}
