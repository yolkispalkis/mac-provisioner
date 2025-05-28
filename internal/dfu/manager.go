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

// Константы VID/PID (дублируются из monitor.go, лучше вынести в общий пакет)
const (
	appleVendorID_DFUManager     = "0x05ac"
	dfuModePIDAS_DFUManager      = "0x1281"
	recoveryModePIDAS_DFUManager = "0x1280"
	dfuModePIDIntelT2_DFUManager = "0x1227"
)

// Структуры для парсинга JSON (дублируются)
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

type Manager struct{}

func New() *Manager { return &Manager{} }

/* ============================================================
   ВХОД В DFU (логика macvdmtool/cfgutil reboot остается)
   ============================================================ */

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
			log.Println("ℹ️ macvdmtool не сработал. Переход к ручному методу DFU.")
			// Возвращаем ошибку, чтобы вызывающий код мог предложить ручной DFU
			return fmt.Errorf("macvdmtool failed: %w, then sudo macvdmtool failed: %w", err, errSudo)
		}
	}

	log.Println("ℹ️ Команда macvdmtool dfu отправлена. Ожидание появления устройства в DFU режиме...")
	return m.WaitForDFUMode(ctx, originalSerial, 2*time.Minute)
}

func (m *Manager) enterDFUWithCfgutilRebootThenManual(ctx context.Context, serialNumber string) error {
	log.Printf("ℹ️ macvdmtool недоступен или не сработал. Попытка 'cfgutil reboot' для %s, затем ручной DFU.", serialNumber)

	cmd := exec.CommandContext(ctx, "cfgutil", "reboot", "-s", serialNumber)
	if err := cmd.Run(); err != nil {
		log.Printf("⚠️ Ошибка 'cfgutil reboot -s %s': %v. Возможно, устройство уже не в подходящем состоянии.", serialNumber, err)
	} else {
		log.Println("✅ Команда 'cfgutil reboot' отправлена. Устройство должно перезагрузиться.")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Возвращаем специальную ошибку или nil, чтобы указать на необходимость ручного DFU
	// m.OfferManualDFU(serialNumber) будет вызван в provisioner.Manager
	return fmt.Errorf("требуется ручной вход в DFU после попытки cfgutil reboot (или macvdmtool не сработал)")
}

// OfferManualDFU выводит инструкции для ручного входа в DFU.
func (m *Manager) OfferManualDFU(serialOrHint string) {
	log.Printf(`ТРЕБУЕТСЯ РУЧНОЙ ВХОД В DFU для устройства (связанного с %s).
Пожалуйста, следуйте инструкциям для вашего типа Mac:

Для Mac на Apple Silicon:
 1. Полностью выключите Mac.
 2. Подключите его к этому компьютеру кабелем USB-C.
 3. Нажмите и удерживайте кнопку питания — удерживайте пока не появится «Загрузка вариантов запуска…»
 4. Отпустите кнопку питания — Mac должен появиться в DFU режиме.

Для Intel Mac (особенно с чипом T2):
 1. Полностью выключите Mac.
 2. Подключите через USB-C/Thunderbolt к этому компьютеру.
 3. Убедитесь, что Mac выключен. Нажмите и удерживайте кнопку питания.
 4. После ~10 секунд, продолжая удерживать кнопку питания, нажмите и удерживайте левую клавишу Shift + левую клавишу Option + левую клавишу Control в течение ~7-10 секунд.
 5. Отпустите все три клавиши (Shift, Option, Control), но продолжайте удерживать кнопку питания, пока Mac не появится в DFU.

После выполнения этих шагов, система попытается автоматически обнаружить устройство в DFU режиме в течение следующих нескольких минут.
`, serialOrHint)
}

/* ============================================================
   WAIT HELPERS (используют system_profiler)
   ============================================================ */

// WaitForDFUMode ожидает появления *любого* устройства в DFU/Recovery.
func (m *Manager) WaitForDFUMode(ctx context.Context, purposeHint string, timeout time.Duration) error {
	log.Printf("⏳ Ожидание появления устройства в DFU/Recovery режиме (для %s), таймаут %v (проверка через system_profiler)...", purposeHint, timeout)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	deadline := time.NewTimer(timeout) // Используем NewTimer для возможности Stop
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("ℹ️ Ожидание DFU отменено через контекст.")
			return ctx.Err()
		case <-deadline.C:
			log.Printf("❌ Таймаут ожидания DFU/Recovery для %s.", purposeHint)
			return fmt.Errorf("устройство (для %s) не вошло в DFU/Recovery режим в течение %v", purposeHint, timeout)
		case <-ticker.C:
			if m.isInDFUMode(ctx) {
				log.Printf("✅ Обнаружено DFU/Recovery устройство (system_profiler). Предполагаем, что это целевое для %s.", purposeHint)
				return nil
			}
			log.Printf("⌛ ...все еще ожидаем DFU/Recovery для %s...", purposeHint)
		}
	}
}

func (m *Manager) isInDFUMode(ctx context.Context) bool {
	devices := m.GetDFUDevices(ctx)
	return len(devices) > 0
}

/* ============================================================
   ЧТЕНИЕ DFU-УСТРОЙСТВ (через system_profiler)
   ============================================================ */

func (m *Manager) GetDFUDevices(ctx context.Context) []*device.Device {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out

	err := cmd.Run()
	if err != nil {
		if ctx.Err() == context.Canceled {
			return nil
		}
		return nil
	}

	var data SPUSBDataType_DFUManager
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return nil
	}

	var dfuDevices []*device.Device
	for _, usbControllerInfo := range data.Items {
		m.extractDFUDevicesRecursive(&usbControllerInfo, &dfuDevices)
	}
	return dfuDevices
}

func (m *Manager) extractDFUDevicesRecursive(spItem *SPUSBItem_DFUManager, devices *[]*device.Device) {
	if strings.EqualFold(spItem.VendorID, appleVendorID_DFUManager) {
		pidLower := strings.ToLower(spItem.ProductID)
		isDFUMode := false
		deviceState := "Unknown"
		deviceModel := spItem.Name

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

		if isDFUMode {
			dev := &device.Device{
				Model:       deviceModel,
				State:       deviceState,
				IsDFU:       true,
				USBLocation: spItem.LocationID,
			}
			if spItem.SerialNum != "" {
				dev.ECID = spItem.SerialNum
				dev.SerialNumber = "DFU-" + strings.TrimPrefix(strings.ToLower(dev.ECID), "0x")
			}
			if dev.ECID != "" {
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
		ecid := devs[0].ECID
		ecid = strings.ToLower(ecid)
		ecid = strings.TrimPrefix(ecid, "0x")
		return ecid
	}
	return ""
}
