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
	USBLocation  string `json:"usb_location,omitempty"` // Добавили поле для USB локации
}

func (d *Device) NeedsProvisioning() bool {
	// Если устройство в DFU режиме, оно точно нуждается в прошивке
	if d.IsDFU {
		return true
	}

	state := strings.ToLower(d.State)

	// Для обычных Mac устройств проверяем состояние
	// Если состояние "Unknown" или пустое, считаем что нужна прошивка
	if state == "unknown" || state == "" || state == "n/a" {
		return true
	}

	// Если устройство не сопряжено и не доступно, нужна прошивка
	if !(state == "paired" || state == "available") {
		return true
	}

	// Также проверяем на состояния, которые указывают на необходимость прошивки
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

// Получает читаемое имя устройства для голосовых уведомлений
func (d *Device) GetFriendlyName() string {
	modelName := d.GetReadableModel()

	if d.USBLocation != "" {
		portNumber := d.extractPortNumber()
		if portNumber != "" {
			return fmt.Sprintf("%s на порту %s", modelName, portNumber)
		}
	}

	return modelName
}

// Преобразует техническое название модели в читаемое
func (d *Device) GetReadableModel() string {
	model := strings.ToLower(d.Model)

	// Преобразуем технические названия в читаемые
	if strings.Contains(model, "macbookair") {
		return "МакБук Эйр"
	}
	if strings.Contains(model, "macbookpro") {
		return "МакБук Про"
	}
	if strings.Contains(model, "macbook") {
		return "МакБук"
	}
	if strings.Contains(model, "imac") {
		return "АйМак"
	}
	if strings.Contains(model, "macmini") {
		return "Мак Мини"
	}
	if strings.Contains(model, "macstudio") {
		return "Мак Студио"
	}
	if strings.Contains(model, "macpro") {
		return "Мак Про"
	}

	// Если не удалось определить, возвращаем оригинальное название
	return d.Model
}

// Извлекает номер порта из USB локации
func (d *Device) extractPortNumber() string {
	if d.USBLocation == "" {
		return ""
	}

	// Ищем паттерны типа "0x14200000", "Location: 0x14200000" и т.д.
	location := strings.ToLower(d.USBLocation)

	// Убираем префиксы
	location = strings.TrimPrefix(location, "location:")
	location = strings.TrimPrefix(location, "0x")
	location = strings.TrimSpace(location)

	// Если это hex число, преобразуем в простой номер порта
	if len(location) >= 8 {
		// Берем последние цифры как номер порта
		portNum := location[len(location)-1:]
		if portNum >= "1" && portNum <= "9" {
			return portNum
		}

		// Альтернативный способ - берем предпоследнюю цифру
		if len(location) >= 2 {
			portNum = location[len(location)-2 : len(location)-1]
			if portNum >= "1" && portNum <= "9" {
				return portNum
			}
		}
	}

	return "неизвестный"
}

func (d *Device) String() string {
	return fmt.Sprintf("Device{SN: %s, Model: %s, State: %s, DFU: %v, USB: %s}",
		d.SerialNumber, d.Model, d.State, d.IsDFU, d.USBLocation)
}
