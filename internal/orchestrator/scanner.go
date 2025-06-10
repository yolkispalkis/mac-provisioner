package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"

	"mac-provisioner/internal/model"
)

// scanUSB выполняет системный вызов для получения списка USB-устройств.
func scanUSB(ctx context.Context) ([]*model.Device, error) {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	var data struct {
		USB []json.RawMessage `json:"SPUSBDataType"`
	}
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return nil, err
	}

	var devices []*model.Device
	for _, rawItem := range data.USB {
		devices = append(devices, parseDeviceTree(rawItem)...)
	}

	return devices, nil
}

// parseDeviceTree рекурсивно обходит дерево USB-устройств из JSON.
func parseDeviceTree(rawItem json.RawMessage) []*model.Device {
	var item struct {
		Name         string            `json:"_name"`
		ProductID    string            `json:"product_id"`
		VendorID     string            `json:"vendor_id"`
		SerialNum    string            `json:"serial_num"`
		LocationID   string            `json:"location_id"`
		Manufacturer string            `json:"manufacturer"`
		Items        []json.RawMessage `json:"_items"`
	}
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return nil
	}

	var found []*model.Device

	// Проверяем, является ли текущий элемент устройством Apple
	if strings.EqualFold(item.VendorID, "0x05ac") || strings.Contains(item.Manufacturer, "Apple") {
		if dev := createDeviceFromProfiler(&item); dev != nil {
			found = append(found, dev)
		}
	}

	// Рекурсивно проверяем дочерние элементы
	for _, subItem := range item.Items {
		found = append(found, parseDeviceTree(subItem)...)
	}

	return found
}

// createDeviceFromProfiler преобразует данные из system_profiler в нашу модель.
func createDeviceFromProfiler(item *struct {
	Name         string            `json:"_name"`
	ProductID    string            `json:"product_id"`
	VendorID     string            `json:"vendor_id"`
	SerialNum    string            `json:"serial_num"`
	LocationID   string            `json:"location_id"`
	Manufacturer string            `json:"manufacturer"`
	Items        []json.RawMessage `json:"_items"`
}) *model.Device {
	dev := &model.Device{
		Name:        item.Name,
		USBLocation: item.LocationID,
		State:       model.StateUnknown,
	}

	name := strings.ToLower(item.Name)
	isDFUProduct := item.ProductID == "0x1281" || item.ProductID == "0x1227"
	isRecoveryProduct := item.ProductID == "0x1280"

	if strings.Contains(name, "dfu mode") || isDFUProduct {
		dev.State = model.StateDFU
	} else if strings.Contains(name, "recovery mode") || isRecoveryProduct {
		dev.State = model.StateRecovery
	} else if item.SerialNum != "" && len(item.SerialNum) > 5 { // Эвристика для обычного Mac
		dev.State = model.StateNormal
	} else {
		return nil // Неизвестное устройство Apple, игнорируем
	}

	// Извлекаем ECID из серийного номера
	re := regexp.MustCompile(`(?i)ECID:? ([0-9A-F]+)`)
	matches := re.FindStringSubmatch(item.SerialNum)
	if len(matches) > 1 {
		dev.ECID = "0x" + matches[1]
	}

	return dev
}
