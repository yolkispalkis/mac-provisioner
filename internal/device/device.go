package device

import (
	"fmt"
	"strings"
)

// Device описывает Mac-устройство в любом режиме (Normal / DFU / Recovery).
// В качестве основных идентификаторов теперь используются:
//
//   - ECID  – если устройство в DFU/Recovery (может быть 0xHEX или decimal)
//   - USBLocation – стабильный идентификатор физического порта
type Device struct {
	ECID        string `json:"ecid,omitempty"` // 0x…  / 123…
	USBLocation string `json:"usb_location"`   // 0x00100000/1
	Model       string `json:"model"`          // MacBookPro18,3 …
	State       string `json:"state"`          // Normal / DFU / Recovery
	IsDFU       bool   `json:"is_dfu"`
}

/*
   ──────────────────────────────────────────────────────────
   Базовые методы
   ──────────────────────────────────────────────────────────
*/

// UniqueID → главный ключ для map/кэшей.
// ECID (без 0x, в lower-case)   или   USBLocation (без 0x).
func (d *Device) UniqueID() string {
	if d.ECID != "" {
		return strings.ToLower(strings.TrimPrefix(d.ECID, "0x"))
	}
	return strings.ToLower(strings.TrimPrefix(d.USBLocation, "0x"))
}

// NeedsProvisioning сообщает, надо ли прошивать устройство
// (готово ли оно к restore или ещё нужно перевести в DFU).
func (d *Device) NeedsProvisioning() bool {
	if d.IsDFU {
		return d.ECID != ""
	}
	return d.IsNormalMac()
}

// IsNormalMac – устройство в рабочем режиме macOS.
func (d *Device) IsNormalMac() bool {
	return !d.IsDFU
}

// IsProvisioned – после прошивки устройство становится «normal».
func (d *Device) IsProvisioned() bool {
	return !d.IsDFU
}

/*
   ──────────────────────────────────────────────────────────
   Читабельные имена / логи
   ──────────────────────────────────────────────────────────
*/

// GetFriendlyName – короткое имя для логов и TTS.
func (d *Device) GetFriendlyName() string {
	if d.IsDFU && d.ECID != "" {
		clean := strings.TrimPrefix(strings.ToLower(d.ECID), "0x")
		return fmt.Sprintf("%s (ECID: …%s)", d.Model, tail(clean, 6))
	}

	if d.USBLocation != "" {
		return fmt.Sprintf("%s (USB: %s)", d.Model, strings.TrimPrefix(d.USBLocation, "0x"))
	}

	return d.Model
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// GetReadableModel – для голосовых уведомлений.
func (d *Device) GetReadableModel() string {
	l := strings.ToLower(d.Model)
	switch {
	case strings.Contains(l, "dfu mode"):
		return "устройство в режиме ДФУ"
	case strings.Contains(l, "recovery mode"):
		return "устройство в режиме восстановления"
	default:
		return d.Model
	}
}

// String – полный вывод для отладки.
func (d *Device) String() string {
	return fmt.Sprintf("Device{ECID:%s, USB:%s, Model:%s, State:%s, DFU:%v}",
		d.ECID, d.USBLocation, d.Model, d.State, d.IsDFU)
}
