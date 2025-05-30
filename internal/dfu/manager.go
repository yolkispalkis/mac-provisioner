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

/*
   ──────────────────────────────────────────────────────────
   Константы / типы – синхронизированы с device/monitor.go
   ──────────────────────────────────────────────────────────
*/

const (
	appleVendorIDHex_DFUManager    = "0x05ac"
	appleVendorIDString_DFUManager = "apple_vendor_id"
	appleManufacturer_DFUManager   = "Apple Inc."

	dfuModePIDAS_DFUManager      = "0x1281"
	recoveryModePIDAS_DFUManager = "0x1280"
	dfuModePIDIntelT2_DFUManager = "0x1227"
)

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

/*
   ──────────────────────────────────────────────────────────
   Вспомогательные функции (общие с monitor)
   ──────────────────────────────────────────────────────────
*/

func extractECIDFromString_DFUManager(s string) string {
	const marker = "ECID:"
	if idx := strings.Index(s, marker); idx != -1 {
		sub := s[idx+len(marker):]
		if end := strings.Index(sub, " "); end != -1 {
			sub = sub[:end]
		}
		return strings.TrimSpace(sub)
	}
	return ""
}

func isAppleDevice_DFUManager(i *SPUSBItem_DFUManager) bool {
	return strings.EqualFold(i.VendorID, appleVendorIDHex_DFUManager) ||
		strings.EqualFold(i.VendorID, appleVendorIDString_DFUManager) ||
		strings.Contains(i.Manufacturer, appleManufacturer_DFUManager)
}

func isDFURecoveryByPID_DFUManager(pid string) (bool, string, string) {
	switch strings.ToLower(pid) {
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
	l := strings.ToLower(name)
	if strings.Contains(l, "dfu mode") {
		return true, "DFU"
	}
	if strings.Contains(l, "recovery mode") {
		return true, "Recovery"
	}
	return false, ""
}

/*
   ──────────────────────────────────────────────────────────
   Manager
   ──────────────────────────────────────────────────────────
*/

type Manager struct{}

func New() *Manager { return &Manager{} }

/*
ВАЖНО: метод теперь принимает usbLocation (ID физического порта),
а не SerialNumber. Если usbLocation неизвестен – передайте пустую
строку, лог всё равно будет корректным.
*/
func (m *Manager) EnterDFUMode(ctx context.Context, usbLocation string) error {
	if m.hasMacvdmtool() {
		return m.enterDFUWithMacvdmtool(ctx, usbLocation)
	}
	return fmt.Errorf("macvdmtool недоступен, автоматический вход в DFU невозможен")
}

func (m *Manager) hasMacvdmtool() bool {
	_, err := exec.LookPath("macvdmtool")
	return err == nil
}

func (m *Manager) enterDFUWithMacvdmtool(ctx context.Context, usbLocation string) error {
	log.Printf("ℹ️ macvdmtool dfu → инициируем переход в DFU (USB %s)…", usbLocation)

	cmd := exec.CommandContext(ctx, "macvdmtool", "dfu")
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ macvdmtool dfu без sudo не удался: %v. Пробуем sudo -n…", err)
		if errSudo := exec.CommandContext(ctx, "sudo", "-n", "macvdmtool", "dfu").Run(); errSudo != nil {
			return fmt.Errorf("macvdmtool failed: %v; sudo macvdmtool failed: %v", err, errSudo)
		}
	}

	log.Println("ℹ️ Команда отправлена. Ожидаем появление DFU-устройства…")
	return m.WaitForDFUMode(ctx, usbLocation, 2*time.Minute)
}

func (m *Manager) OfferManualDFU(portHint string) {
	log.Printf(`
🔧 РУЧНОЙ DFU

Устройство на порту %s не удалось перевести автоматически.
Следуйте инструкции, чтобы ввести Mac в DFU/Recovery, затем 
программа продолжит работу автоматически.
`, portHint)
}

/*
WaitForDFUMode ждёт, пока появится ≥1 DFU-девайс.
purposeHint – произвольная строка, выводится в логах.
*/
func (m *Manager) WaitForDFUMode(ctx context.Context, purposeHint string, timeout time.Duration) error {
	log.Printf("⏳ Ждём DFU (%s), таймаут %v…", purposeHint, timeout)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("устройству (%s) не удалось войти в DFU за %v", purposeHint, timeout)
		case <-ticker.C:
			if m.isInDFUMode(ctx) {
				log.Printf("✅ DFU-устройство обнаружено (%s)", purposeHint)
				return nil
			}
		}
	}
}

func (m *Manager) isInDFUMode(ctx context.Context) bool {
	return len(m.GetDFUDevices(ctx)) > 0
}

/*
   ──────────────────────────────────────────────────────────
   Сканирование USB
   ──────────────────────────────────────────────────────────
*/

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

	var dfu []*device.Device
	for i := range data.Items {
		m.extractDFUDevicesRecursive(&data.Items[i], &dfu)
	}
	return dfu
}

func (m *Manager) extractDFUDevicesRecursive(sp *SPUSBItem_DFUManager, acc *[]*device.Device) {
	if !isAppleDevice_DFUManager(sp) {
		for i := range sp.SubItems {
			m.extractDFUDevicesRecursive(&sp.SubItems[i], acc)
		}
		return
	}

	var dev *device.Device

	if isDFU, state, model := isDFURecoveryByPID_DFUManager(sp.ProductID); isDFU {
		dev = &device.Device{
			Model:       model,
			State:       state,
			IsDFU:       true,
			USBLocation: sp.LocationID,
		}
		if ecid := extractECIDFromString_DFUManager(sp.SerialNum); ecid != "" {
			dev.ECID = ecid
		}
	} else if isDFU, state := isDFURecoveryByName_DFUManager(sp.Name); isDFU {
		dev = &device.Device{
			Model:       sp.Name,
			State:       state,
			IsDFU:       true,
			USBLocation: sp.LocationID,
		}
		if ecid := extractECIDFromString_DFUManager(sp.SerialNum); ecid != "" {
			dev.ECID = ecid
		}
	}

	if dev != nil && dev.ECID != "" {
		*acc = append(*acc, dev)
	}

	for i := range sp.SubItems {
		m.extractDFUDevicesRecursive(&sp.SubItems[i], acc)
	}
}

// GetFirstDFUECID – удобный хелпер.
func (m *Manager) GetFirstDFUECID(ctx context.Context) string {
	if devs := m.GetDFUDevices(ctx); len(devs) > 0 && devs[0].ECID != "" {
		return devs[0].ECID
	}
	return ""
}
