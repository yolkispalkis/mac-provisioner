package device

import (
	"strings"
)

type Device struct {
	ECID         string `json:"ecid"`
	USBLocation  string `json:"usb_location"`
	Name         string `json:"name"`          // Исходное имя из system_profiler
	ResolvedName string `json:"resolved_name"` // Красивое имя из cfgutil
	State        string `json:"state"`
	IsDFU        bool   `json:"is_dfu"`
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

// GetDisplayName возвращает лучшее доступное имя устройства
func (d *Device) GetDisplayName() string {
	if d.ResolvedName != "" {
		return d.ResolvedName
	}
	return d.Name
}

// GetReadableName возвращает имя для голосовых уведомлений
func (d *Device) GetReadableName() string {
	name := d.GetDisplayName()

	// Обрабатываем специальные случаи для TTS
	name = strings.ToLower(name)
	if strings.Contains(name, "dfu mode") {
		return "устройство в режиме ДФУ"
	}
	if strings.Contains(name, "recovery mode") {
		return "устройство в режиме восстановления"
	}

	return d.GetDisplayName()
}
