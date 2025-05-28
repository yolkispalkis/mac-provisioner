package device

import "strings"

type Device struct {
	SerialNumber string `json:"serial_number"`
	Model        string `json:"model"`
	State        string `json:"state"`
	IsDFU        bool   `json:"is_dfu"`
	ECID         string `json:"ecid,omitempty"`
}

func (d *Device) NeedsProvisioning() bool {
	if d.IsDFU {
		return true
	}

	state := strings.ToLower(d.State)
	return !(state == "paired" || state == "available")
}

func (d *Device) IsProvisioned() bool {
	if d.IsDFU {
		return false
	}

	state := strings.ToLower(d.State)
	return state == "paired" || state == "available"
}

func (d *Device) IsValidSerial() bool {
	if len(d.SerialNumber) < 8 || len(d.SerialNumber) > 20 {
		return false
	}

	// Проверяем, что серийный номер не содержит артефакты
	invalid := []string{"ECID", "0x", "Type:", "N/A", "Unknown"}
	for _, inv := range invalid {
		if strings.Contains(d.SerialNumber, inv) {
			return false
		}
	}

	return true
}
