package dfu

import (
	"context"
	"errors"
	"log"
	"os/exec"
	"strings"
	"time"
)

type Manager struct{}

func New() *Manager {
	return &Manager{}
}

func (m *Manager) EnterDFUMode(ctx context.Context, usbLocation string) error {
	if !m.canEnterDFU(usbLocation) {
		return errors.New("автоматический вход в DFU невозможен")
	}

	if !m.hasMacvdmtool() {
		return errors.New("macvdmtool недоступен")
	}

	log.Printf("🔄 Инициируем переход в DFU режим...")

	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		return err
	}

	return m.waitForDFUMode(ctx, 2*time.Minute)
}

func (m *Manager) canEnterDFU(usbLocation string) bool {
	if usbLocation == "" {
		return false
	}

	// Проверяем, что это основной USB порт
	parts := strings.Split(usbLocation, "/")
	if len(parts) == 0 {
		return false
	}

	base := strings.ToLower(strings.TrimPrefix(parts[0], "0x"))
	return strings.HasPrefix(base, "00100000")
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) waitForDFUMode(ctx context.Context, timeout time.Duration) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeoutTimer.C:
			return errors.New("таймаут ожидания DFU режима")
		case <-ticker.C:
			if m.isDFUModeActive(ctx) {
				log.Printf("✅ Устройство перешло в DFU режим")
				return nil
			}
		}
	}
}

func (m *Manager) isDFUModeActive(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	outputStr := strings.ToLower(string(output))
	return strings.Contains(outputStr, "dfu mode")
}
