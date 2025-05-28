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
}

func (d *Device) NeedsProvisioning() bool {
	// Если устройство в DFU режиме, оно точно нуждается в прошивке
	if d.IsDFU {
		return true
	}

	state := strings.ToLower(d.State)

	// Устройство нуждается в прошивке, если оно не сопряжено и не доступно
	needsProvisioning := !(state == "paired" || state == "available")

	// Также проверяем на состояния, которые указывают на необходимость прошивки
	if strings.Contains(state, "recovery") ||
		strings.Contains(state, "restore") ||
		strings.Contains(state, "dfu") ||
		state == "unknown" ||
		state == "" {
		needsProvisioning = true
	}

	return needsProvisioning
}

func (d *Device) IsProvisioned() bool {
	if d.IsDFU {
		return false
	}

	state := strings.ToLower(d.State)
	return state == "paired" || state == "available"
}

func (d *Device) IsValidSerial() bool {
	if d.SerialNumber == "" {
		return false
	}

	// Для DFU устройств разрешаем серийные номера с префиксом DFU-
	if strings.HasPrefix(d.SerialNumber, "DFU-") {
		return len(d.SerialNumber) > 4 // DFU- + что-то еще
	}

	if len(d.SerialNumber) < 8 || len(d.SerialNumber) > 20 {
		return false
	}

	// Проверяем, что серийный номер не содержит артефакты
	invalid := []string{"ECID", "0x", "Type:", "N/A", "Unknown", "Name"}
	for _, inv := range invalid {
		if strings.Contains(d.SerialNumber, inv) {
			return false
		}
	}

	return true
}

func (d *Device) String() string {
	return fmt.Sprintf("Device{SN: %s, Model: %s, State: %s, DFU: %v}",
		d.SerialNumber, d.Model, d.State, d.IsDFU)
}
