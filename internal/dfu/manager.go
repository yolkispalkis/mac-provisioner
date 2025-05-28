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

	"mac-provisioner/internal/device"
)

// Константы для Apple устройств (синхронизированы с monitor.go)
const (
	appleVendorIDHex_DFUManager    = "0x05ac"
	appleVendorIDString_DFUManager = "apple_vendor_id"
	appleManufacturer_DFUManager   = "Apple Inc."
	dfuModePIDAS_DFUManager        = "0x1281"
	recoveryModePIDAS_DFUManager   = "0x1280"
	dfuModePIDIntelT2_DFUManager   = "0x1227"
)

// Структуры для парсинга JSON (синхронизированы с monitor.go)
type SPUSBItem_DFUManager struct {
	Name         string                 `json:"_name"`
	ProductID    string                 `json:"product_id,omitempty"`
	VendorID     string                 `json:"vendor_id,omitempty"`
	SerialNum    string                 `json:"serial_num,omitempty"`
	LocationID   string                 `json:"location_id,omitempty"`
	Manufacturer string                 `json:"manufacturer,omitempty"`
	SubItems     []SPUSBItem_DFUManager `json:"_items,omitempty"`
}

type SPUSBDataType_DFUManager struct {
	Items []SPUSBItem_DFUManager `json:"SPUSBDataType"`
}

// Вспомогательные функции (синхронизированы с monitor.go)
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

func isAppleDevice_DFUManager(item *SPUSBItem_DFUManager) bool {
	return strings.EqualFold(item.VendorID, appleVendorIDHex_DFUManager) ||
		strings.EqualFold(item.VendorID, appleVendorIDString_DFUManager) ||
		strings.Contains(item.Manufacturer, appleManufacturer_DFUManager)
}

func isDFURecoveryByPID_DFUManager(productID string) (bool, string, string) {
	pidLower := strings.ToLower(productID)
	switch pidLower {
	case dfuModePIDAS_DFUManager:
		return true, "DFU", "Apple Silicon (DFU Mode)"
	case recoveryModePIDAS_DFUManager:
		return true, "Recovery", "Apple Silicon (Recovery Mode)"
	case dfuModePIDIntelT2_DFUManager:
		return true, "DFU", "Intel T2 (DFU Mode)"
	}
	return false, "", ""
}

func isDFURecoveryByName_DFUManager(name string) (bool, string) {
	nameLower := strings.ToLower(name)
	if strings.Contains(nameLower, "dfu mode") {
		return true, "DFU"
	}
	if strings.Contains(nameLower, "recovery mode") {
		return true, "Recovery"
	}
	return false, ""
}

type Manager struct{}

func New() *Manager { return &Manager{} }

func (m *Manager) EnterDFUMode(ctx context.Context, serialNumber string) error {
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(ctx, serialNumber)
	}
	return fmt.Errorf("macvdmtool недоступен, автоматический вход в DFU невозможен")
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

func (m *Manager) OfferManualDFU(serialOrHint string) {
	log.Printf(`
🔧 ТРЕБУЕТСЯ РУЧНОЙ ВХОД В DFU для устройства %s

Инструкции для входа в DFU режим:

Apple Silicon Mac (M1/M2/M3):
1. Выключите Mac полностью
2. Нажмите и удерживайте кнопку питания
3. Продолжайте удерживать до появления окна параметров загрузки
4. Нажмите "Параметры", затем "Продолжить"
5. В Утилитах выберите "Утилиты" > "Восстановить систему безопасности"

Intel Mac с T2:
1. Выключите Mac полностью
2. Нажмите и удерживайте правый Shift + левый Control + левый Option
3. Удерживая эти клавиши, нажмите и удерживайте кнопку питания
4. Удерживайте все клавиши 10 секунд, затем отпустите

После входа в DFU режим устройство будет автоматически обнаружено.
`, serialOrHint)
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
	if !isAppleDevice_DFUManager(spItem) {
		if spItem.SubItems != nil {
			for i := range spItem.SubItems {
				m.extractDFUDevicesRecursive(&spItem.SubItems[i], devices)
			}
		}
		return
	}

	var dev *device.Device

	// Проверяем DFU/Recovery по PID
	if isDFU, state, model := isDFURecoveryByPID_DFUManager(spItem.ProductID); isDFU {
		dev = &device.Device{
			Model: model, State: state, IsDFU: true, USBLocation: spItem.LocationID,
		}
		parsedECID := extractECIDFromString_DFUManager(spItem.SerialNum)
		if parsedECID != "" {
			dev.ECID = parsedECID
			dev.SerialNumber = "DFU-" + strings.ToLower(dev.ECID)
		}
	} else if isDFU, state := isDFURecoveryByName_DFUManager(spItem.Name); isDFU {
		// Проверяем DFU/Recovery по имени (fallback)
		dev = &device.Device{
			Model: spItem.Name, State: state, IsDFU: true, USBLocation: spItem.LocationID,
		}
		parsedECID := extractECIDFromString_DFUManager(spItem.SerialNum)
		if parsedECID != "" {
			dev.ECID = parsedECID
			dev.SerialNumber = "DFU-" + strings.ToLower(dev.ECID)
		}
	}

	if dev != nil && dev.ECID != "" && dev.IsValidSerial() {
		*devices = append(*devices, dev)
	}

	if spItem.SubItems != nil {
		for i := range spItem.SubItems {
			m.extractDFUDevicesRecursive(&spItem.SubItems[i], devices)
		}
	}
}

func (m *Manager) GetFirstDFUECID(ctx context.Context) string {
	if devs := m.GetDFUDevices(ctx); len(devs) > 0 && devs[0].ECID != "" {
		return devs[0].ECID
	}
	return ""
}
