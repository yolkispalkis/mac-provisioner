package dfu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"mac-provisioner/internal/device" // Используется для device.Device
)

// Обновленные константы для Vendor ID (должны быть синхронизированы с monitor.go)
const (
	appleVendorIDHex_DFUManager    = "0x05ac"
	appleVendorIDString_DFUManager = "apple_vendor_id"
	dfuModePIDAS_DFUManager        = "0x1281"
	recoveryModePIDAS_DFUManager   = "0x1280"
	dfuModePIDIntelT2_DFUManager   = "0x1227"
)

// Структуры для парсинга JSON (должны быть синхронизированы с monitor.go)
type SPUSBItem_DFUManager struct {
	Name       string                 `json:"_name"`
	ProductID  string                 `json:"product_id,omitempty"`
	VendorID   string                 `json:"vendor_id,omitempty"`
	SerialNum  string                 `json:"serial_num,omitempty"`
	LocationID string                 `json:"location_id,omitempty"`
	SubItems   []SPUSBItem_DFUManager `json:"_items,omitempty"`
}

type SPUSBDataType_DFUManager struct {
	Items []SPUSBItem_DFUManager `json:"SPUSBDataType"`
}

// Вспомогательная функция для извлечения ECID (должна быть синхронизирована с monitor.go)
func extractECIDFromString_DFUManager(s string) string {
	marker := "ECID:"
	index := strings.Index(s, marker)
	if index == -1 {
		return ""
	}
	sub := s[index+len(marker):]
	endIndex := strings.Index(sub, " ")
	if endIndex == -1 {
		return strings.TrimSpace(sub)
	}
	return strings.TrimSpace(sub[:endIndex])
}

type Manager struct{}

func New() *Manager { return &Manager{} }

func (m *Manager) EnterDFUMode(ctx context.Context, serialNumber string) error {
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(ctx, serialNumber)
	}
	return m.enterDFUWithCfgutilRebootThenManual(ctx, serialNumber)
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(ctx context.Context, originalSerial string) error {
	log.Printf("ℹ️ Попытка входа в DFU для %s с помощью macvdmtool...", originalSerial)

	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ macvdmtool dfu без sudo не удался: %v. Пробуем с sudo -n...", err)
		cmdSudo := exec.CommandContext(ctx, "sudo", "-n", "macvdmtool", "dfu")
		if errSudo := cmdSudo.Run(); errSudo != nil {
			log.Printf("❌ Ошибка выполнения 'sudo -n macvdmtool dfu': %v", errSudo)
			return fmt.Errorf("macvdmtool failed: %v, then sudo macvdmtool failed: %v", err, errSudo)
		}
	}
	log.Println("ℹ️ Команда macvdmtool dfu отправлена. Ожидание появления устройства в DFU режиме...")
	return m.WaitForDFUMode(ctx, originalSerial, 2*time.Minute)
}

func (m *Manager) enterDFUWithCfgutilRebootThenManual(ctx context.Context, serialNumber string) error {
	log.Printf("ℹ️ macvdmtool недоступен или не сработал. Попытка 'cfgutil reboot' для %s, затем ручной DFU.", serialNumber)
	cmd := exec.CommandContext(ctx, "cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ Ошибка 'cfgutil reboot -s %s': %v.", serialNumber, err)
	} else {
		log.Println("✅ Команда 'cfgutil reboot' отправлена.")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("требуется ручной вход в DFU") // Сигнализируем о необходимости ручных действий
}

func (m *Manager) OfferManualDFU(serialOrHint string) {
	log.Printf(`ТРЕБУЕТСЯ РУЧНОЙ ВХОД В DFU для устройства (связанного с %s).
(Инструкции)...`, serialOrHint) // Сокращено для краткости
}

func (m *Manager) WaitForDFUMode(ctx context.Context, purposeHint string, timeout time.Duration) error {
	log.Printf("⏳ Ожидание появления устройства в DFU/Recovery режиме (для %s), таймаут %v...", purposeHint, timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("устройство (для %s) не вошло в DFU/Recovery за %v", purposeHint, timeout)
		case <-ticker.C:
			if m.isInDFUMode(ctx) {
				log.Printf("✅ Обнаружено DFU/Recovery устройство (для %s).", purposeHint)
				return nil
			}
			// log.Printf("⌛ ...все еще ожидаем DFU/Recovery для %s...", purposeHint) // Убрал, чтобы не спамить
		}
	}
}

func (m *Manager) isInDFUMode(ctx context.Context) bool {
	devices := m.GetDFUDevices(ctx)
	return len(devices) > 0
}

func (m *Manager) GetDFUDevices(ctx context.Context) []*device.Device {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// Не логируем ошибки здесь агрессивно, т.к. функция часто вызывается
		return nil
	}
	var data SPUSBDataType_DFUManager
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return nil
	}
	var dfuDevices []*device.Device
	for _, item := range data.Items {
		m.extractDFUDevicesRecursive(&item, &dfuDevices)
	}
	return dfuDevices
}

func (m *Manager) extractDFUDevicesRecursive(spItem *SPUSBItem_DFUManager, devices *[]*device.Device) {
	// log.Printf("DEBUG_USB_DFUMGR: Checking item: Name='%s', VID='%s', PID='%s', SN_Raw='%s'", spItem.Name, spItem.VendorID, spItem.ProductID, spItem.SerialNum)
	isApple := strings.EqualFold(spItem.VendorID, appleVendorIDHex_DFUManager) || strings.EqualFold(spItem.VendorID, appleVendorIDString_DFUManager)

	if isApple {
		// log.Printf("DEBUG_USB_DFUMGR: Apple VID ('%s') matched. Name='%s', PID='%s'", spItem.VendorID, spItem.Name, spItem.ProductID)
		pidLower := strings.ToLower(spItem.ProductID)
		isDFUMode := false
		deviceState := "Unknown"
		deviceModel := spItem.Name

		// matchedPID был здесь, но удален, так как не используется
		switch pidLower {
		case dfuModePIDAS_DFUManager:
			isDFUMode = true
			deviceState = "DFU"
			deviceModel = "Apple Silicon (DFU Mode)"
		case recoveryModePIDAS_DFUManager:
			isDFUMode = true
			deviceState = "Recovery"
			deviceModel = "Apple Silicon (Recovery Mode)"
		case dfuModePIDIntelT2_DFUManager:
			isDFUMode = true
			deviceState = "DFU"
			deviceModel = "Intel T2 (DFU Mode)"
		}

		// if !isDFUMode && isApple {
		//     log.Printf("DEBUG_USB_DFUMGR: Apple device, but PID '%s' for '%s' did not match DFU/Recovery PIDs.", pidLower, spItem.Name)
		// }

		if isDFUMode {
			dev := &device.Device{
				Model: deviceModel, State: deviceState, IsDFU: true, USBLocation: spItem.LocationID,
			}
			parsedECID := extractECIDFromString_DFUManager(spItem.SerialNum)
			if parsedECID != "" {
				dev.ECID = parsedECID
				dev.SerialNumber = "DFU-" + strings.ToLower(dev.ECID)
				// log.Printf("DEBUG_USB_DFUMGR: DFU/Recovery device created: SN='%s', Model='%s', ECID='%s'", dev.SerialNumber, dev.Model, dev.ECID)
			} else {
				// Не логируем здесь, чтобы не дублировать логи монитора
			}
			if dev.ECID != "" && dev.IsValidSerial() { // IsValidSerial из пакета device
				*devices = append(*devices, dev)
			}
		}
	}
	if spItem.SubItems != nil {
		for i := range spItem.SubItems {
			m.extractDFUDevicesRecursive(&spItem.SubItems[i], devices)
		}
	}
}

func (m *Manager) GetFirstDFUECID(ctx context.Context) string {
	if devs := m.GetDFUDevices(ctx); len(devs) > 0 && devs[0].ECID != "" {
		// Возвращаем ECID как есть (HEX строка без 0x), нормализация будет в provisioner
		return devs[0].ECID
	}
	return ""
}
