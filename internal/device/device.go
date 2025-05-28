package device

import (
	"fmt"
	"strings"
)

type Device struct {
	SerialNumber string `json:"serial_number"`
	Model        string `json:"model"`
	State        string `json:"state"`
	IsDFU        bool   `json:"is_dfu"`
	ECID         string `json:"ecid,omitempty"`
	USBLocation  string `json:"usb_location,omitempty"`
}

func (d *Device) NeedsProvisioning() bool {
	// Если уже в DFU - готов к прошивке (нужен ECID)
	if d.IsDFU {
		return d.ECID != ""
	}
	// Если обычный Mac - нужно перевести в DFU
	return d.IsNormalMac()
}

func (d *Device) IsNormalMac() bool {
	return !d.IsDFU && d.SerialNumber != "" && d.IsValidSerial()
}

func (d *Device) IsProvisioned() bool {
	return !d.IsDFU
}

func (d *Device) IsValidSerial() bool {
	if d.SerialNumber == "" {
		return false
	}
	if strings.HasPrefix(d.SerialNumber, "DFU-") {
		return len(d.SerialNumber) > 4
	}
	// Логика для обычных серийных номеров Mac
	if len(d.SerialNumber) < 8 || len(d.SerialNumber) > 20 {
		return false
	}
	invalidSubstrings := []string{"ECID", "0x", "Type:", "N/A", "Unknown", "Name"}
	for _, inv := range invalidSubstrings {
		if strings.Contains(d.SerialNumber, inv) {
			return false
		}
	}
	return true
}

func (d *Device) GetFriendlyName() string {
	if d.IsDFU && d.ECID != "" {
		cleanECID := strings.TrimPrefix(strings.ToLower(d.ECID), "0x")
		return fmt.Sprintf("%s (ECID: ...%s)", d.Model, getLastNChars(cleanECID, 6))
	}
	if d.SerialNumber != "" {
		return fmt.Sprintf("%s (SN: %s)", d.Model, d.SerialNumber)
	}
	return d.Model
}

func getLastNChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (d *Device) GetReadableModel() string {
	modelLower := strings.ToLower(d.Model)
	if strings.Contains(modelLower, "dfu mode") {
		return "устройство в режиме ДФУ"
	}
	if strings.Contains(modelLower, "recovery mode") {
		return "устройство в режиме восстановления"
	}
	return d.Model
}

func (d *Device) String() string {
	return fmt.Sprintf("Device{SN:%s, Model:%s, State:%s, DFU:%v, ECID:%s, USBLoc:%s}",
		d.SerialNumber, d.Model, d.State, d.IsDFU, d.ECID, d.USBLocation)
}
