package model

import "strings"

type State string

const (
	StateUnknown  State = "Unknown"
	StateNormal   State = "Normal"
	StateRecovery State = "Recovery"
	StateDFU      State = "DFU"
)

// Device представляет одно устройство Apple.
type Device struct {
	ECID        string `json:"ecid"`
	USBLocation string `json:"usb_location"`
	Name        string `json:"name"`
	State       State  `json:"state"`
}

// ID возвращает уникальный идентификатор для устройства.
// ECID является предпочтительным, так как он не меняется.
func (d *Device) ID() string {
	if d.ECID != "" {
		return d.ECID
	}
	// USBLocation как запасной вариант для устройств без определенного ECID.
	return d.USBLocation
}

// GetDisplayName возвращает лучшее доступное имя для отображения.
func (d *Device) GetDisplayName() string {
	if d.Name != "" {
		return d.Name
	}
	if d.ECID != "" {
		return "Устройство " + d.ECID
	}
	return "Неизвестное устройство"
}

// GetReadableName возвращает имя, подходящее для синтезатора речи.
func (d *Device) GetReadableName() string {
	name := d.GetDisplayName()
	lowerName := strings.ToLower(name)
	if strings.Contains(lowerName, "dfu") {
		return "устройство в режиме ДФУ"
	}
	if strings.Contains(lowerName, "recovery") {
		return "устройство в режиме восстановления"
	}
	return name
}
