package device

import (
	"fmt"
	"strings"
)

// Device описывает Mac-устройство (Normal / DFU / Recovery).
// Единственный постоянный идентификатор — UniqueID():
//   - ECID (когда есть)   • USBLocation (для Normal-Mac).
type Device struct {
	ECID        string `json:"ecid,omitempty"` // 0x…  / 123…
	USBLocation string `json:"usb_location"`   // 0x00100000/1
	Model       string `json:"model"`          // MacBookPro18,3 …
	State       string `json:"state"`          // Normal / DFU / Recovery
	IsDFU       bool   `json:"is_dfu"`
}

/*──────────────────────────────────────────────────────────
  Базовые методы
  ──────────────────────────────────────────────────────────*/

// UniqueID → ключ для map/кэшей.
func (d *Device) UniqueID() string {
	if d.ECID != "" {
		return strings.ToLower(strings.TrimPrefix(d.ECID, "0x"))
	}
	return strings.ToLower(strings.TrimPrefix(d.USBLocation, "0x"))
}

// Нужно ли прошивать?
func (d *Device) NeedsProvisioning() bool {
	if d.IsDFU {
		return d.ECID != ""
	}
	return d.IsNormalMac()
}

func (d *Device) IsNormalMac() bool   { return !d.IsDFU }
func (d *Device) IsProvisioned() bool { return !d.IsDFU }

/*──────────────────────────────────────────────────────────
  Представление для логов / TTS
  ──────────────────────────────────────────────────────────*/

// GetFriendlyName раньше писал «(ECID: …xxxx)» — убрали.
// Для DFU/Recovery выводим только модель; для Normal-Mac
// оставляем USB-порт (удобно отличать несколько устройств).
func (d *Device) GetFriendlyName() string {
	if d.IsDFU {
		return d.Model // без ECID-хвоста
	}
	if d.USBLocation != "" {
		return fmt.Sprintf("%s (USB: %s)",
			d.Model, strings.TrimPrefix(d.USBLocation, "0x"))
	}
	return d.Model
}

// GetReadableModel — для голосовых уведомлений (коротко).
func (d *Device) GetReadableModel() string {
	l := strings.ToLower(d.Model)
	switch {
	case strings.Contains(l, "dfu mode"):
		return "в режиме ДФУ"
	case strings.Contains(l, "recovery mode"):
		return "в режиме восстановления"
	default:
		return d.Model
	}
}

// Строка для детальной отладки.
func (d *Device) String() string {
	return fmt.Sprintf("Device{UID:%s, Model:%s, State:%s, DFU:%v, ECID:%s, USB:%s}",
		d.UniqueID(), d.Model, d.State, d.IsDFU, d.ECID, d.USBLocation)
}
