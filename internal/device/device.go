package device

import "strings"

type Device struct {
	ECID        string `json:"ecid"`
	USBLocation string `json:"usb_location"`
	Name        string `json:"name"`
	State       string `json:"state"`
	IsDFU       bool   `json:"is_dfu"`
}

func (d *Device) UniqueID() string {
	if d.ECID != "" {
		return strings.ToLower(strings.TrimPrefix(d.ECID, "0x"))
	}
	return strings.ToLower(d.USBLocation)
}

func (d *Device) IsNormalMac() bool {
	return !d.IsDFU && d.State == "Normal"
}

func (d *Device) GetReadableName() string {
	name := d.Name
	if strings.Contains(strings.ToLower(name), "dfu mode") {
		return "устройство в режиме DFU"
	}
	if strings.Contains(strings.ToLower(name), "recovery mode") {
		return "устройство в режиме восстановления"
	}
	return name
}
