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

/* ============================================================
   НУЖНО ЛИ ПРОШИВАТЬ?
   ------------------------------------------------------------
   Новая логика: прошивать ВСЕХ, кроме тех, кто УЖЕ в DFU-процессе
   и у кого явно проставлен "IsProvisioned".
   ============================================================ */

func (d *Device) NeedsProvisioning() bool {
	// 1. Если уже DFU  → да, естественно, нужна прошивка
	if d.IsDFU {
		return true
	}

	// 2. Состояние «paired | available» считаем ГОДНЫМ
	state := strings.ToLower(strings.TrimSpace(d.State))
	if state == "paired" || state == "available" {
		return false // Mac считается готовым, перепрошивка не нужна
	}

	// 3. Любое другое состояние → прошиваем
	return true
}

func (d *Device) IsProvisioned() bool {
	if d.IsDFU {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(d.State))
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

/* ============================================================
   ДРУЖЕСКОЕ ИМЯ
   ============================================================ */

func (d *Device) GetFriendlyName() string {
	model := d.GetReadableModel()
	if port := d.extractPortNumber(); port != "" {
		return fmt.Sprintf("%s на порту %s", model, port)
	}
	return model
}

func (d *Device) GetReadableModel() string {
	m := strings.ToLower(d.Model)
	switch {
	case strings.Contains(m, "macbookair"):
		return "МакБук Эйр"
	case strings.Contains(m, "macbookpro"):
		return "МакБук Про"
	case strings.Contains(m, "macbook"):
		return "МакБук"
	case strings.Contains(m, "imac"):
		return "АйМак"
	case strings.Contains(m, "macmini"):
		return "Мак Мини"
	case strings.Contains(m, "macstudio"):
		return "Мак Студио"
	case strings.Contains(m, "macpro"):
		return "Мак Про"
	default:
		return d.Model
	}
}

func (d *Device) extractPortNumber() string {
	if d.USBLocation == "" {
		return ""
	}
	loc := strings.ToLower(d.USBLocation)
	loc = strings.TrimPrefix(loc, "location:")
	loc = strings.TrimPrefix(loc, "0x")
	loc = strings.TrimSpace(loc)

	if len(loc) >= 8 {
		last := loc[len(loc)-1:]
		if last >= "1" && last <= "9" {
			return last
		}
		if len(loc) >= 2 {
			prev := loc[len(loc)-2 : len(loc)-1]
			if prev >= "1" && prev <= "9" {
				return prev
			}
		}
	}
	return ""
}

func (d *Device) String() string {
	return fmt.Sprintf("Device{SN:%s, Model:%s, State:%s, DFU:%v, USB:%s}",
		d.SerialNumber, d.Model, d.State, d.IsDFU, d.USBLocation)
}
