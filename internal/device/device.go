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

/* ====================== ЛОГИКА СОСТОЯНИЙ ====================== */

func (d *Device) NeedsProvisioning() bool {
	if d.IsDFU {
		return true
	}

	state := strings.ToLower(d.State)

	if state == "unknown" || state == "" || state == "n/a" {
		return true
	}
	if !(state == "paired" || state == "available") {
		return true
	}
	if strings.Contains(state, "recovery") ||
		strings.Contains(state, "restore") ||
		strings.Contains(state, "dfu") {
		return true
	}

	return false
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
	if strings.HasPrefix(d.SerialNumber, "DFU-") {
		return len(d.SerialNumber) > 4
	}
	if len(d.SerialNumber) < 8 || len(d.SerialNumber) > 20 {
		return false
	}
	invalid := []string{"ECID", "0x", "Type:", "N/A", "Unknown", "Name"}
	for _, inv := range invalid {
		if strings.Contains(d.SerialNumber, inv) {
			return false
		}
	}
	return true
}

/* ====================== «ЧИТАЕМОЕ» ИМЯ ====================== */

// Получает читаемое имя устройства для голосовых уведомлений
func (d *Device) GetFriendlyName() string {
	modelName := d.GetReadableModel()

	if port := d.extractPortNumber(); port != "" {
		return fmt.Sprintf("%s на порту %s", modelName, port)
	}
	return modelName
}

// Преобразует техническое название модели в читаемое
func (d *Device) GetReadableModel() string {
	model := strings.ToLower(d.Model)

	switch {
	case strings.Contains(model, "macbookair"):
		return "МакБук Эйр"
	case strings.Contains(model, "macbookpro"):
		return "МакБук Про"
	case strings.Contains(model, "macbook"):
		return "МакБук"
	case strings.Contains(model, "imac"):
		return "АйМак"
	case strings.Contains(model, "macmini"):
		return "Мак Мини"
	case strings.Contains(model, "macstudio"):
		return "Мак Студио"
	case strings.Contains(model, "macpro"):
		return "Мак Про"
	default:
		return d.Model
	}
}

// Извлекает номер порта из USB-location. Если не получилось — возвращает ""
func (d *Device) extractPortNumber() string {
	if d.USBLocation == "" {
		return ""
	}

	location := strings.ToLower(d.USBLocation)
	location = strings.TrimPrefix(location, "location:")
	location = strings.TrimPrefix(location, "0x")
	location = strings.TrimSpace(location)

	if len(location) >= 8 {
		// Последний символ
		portNum := location[len(location)-1:]
		if portNum >= "1" && portNum <= "9" {
			return portNum
		}
		// Предпоследний символ
		if len(location) >= 2 {
			portNum = location[len(location)-2 : len(location)-1]
			if portNum >= "1" && portNum <= "9" {
				return portNum
			}
		}
	}
	return ""
}

func (d *Device) String() string {
	return fmt.Sprintf("Device{SN:%s, Model:%s, State:%s, DFU:%v, USB:%s}",
		d.SerialNumber, d.Model, d.State, d.IsDFU, d.USBLocation)
}
