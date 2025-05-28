package device

import "strings"

type Device struct {
	SerialNumber string `json:"serial_number"`
	Model        string `json:"model"`
	State        string `json:"state"`
	IsDFU        bool   `json:"is_dfu"`
}

func (d *Device) IsProvisioned() bool {
	if d.IsDFU {
		return false
	}

	state := strings.ToLower(d.State)
	return state == "paired" || state == "available"
}
