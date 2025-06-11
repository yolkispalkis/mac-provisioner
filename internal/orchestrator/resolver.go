package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ResolvedInfo содержит точные данные об устройстве из cfgutil.
type ResolvedInfo struct {
	ECID       string
	DeviceType string
	Name       string
}

// Resolver вызывает `cfgutil` для получения точных данных об устройствах.
type Resolver struct{}

func NewResolver() *Resolver {
	return &Resolver{}
}

// GetInfoByLocation выполняет `cfgutil list` и возвращает карту [LocationID -> Info].
func (r *Resolver) GetInfoByLocation(ctx context.Context) (map[string]ResolvedInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second) // Таймаут на вызов
	defer cancel()

	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// Ошибка "нет устройств" (код 70) - не является ошибкой для нас.
		if strings.Contains(err.Error(), "exit status 70") {
			return make(map[string]ResolvedInfo), nil // Возвращаем пустую карту
		}
		return nil, fmt.Errorf("ошибка выполнения cfgutil list: %w", err)
	}

	var response struct {
		Output map[string]struct {
			LocationID int     `json:"locationID"`
			ECID       string  `json:"ECID"`
			Name       *string `json:"name"`
			DeviceType string  `json:"deviceType"`
		} `json:"Output"`
	}

	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		return nil, fmt.Errorf("ошибка парсинга JSON от cfgutil: %w", err)
	}

	resolvedMap := make(map[string]ResolvedInfo)
	for _, devInfo := range response.Output {
		if devInfo.LocationID == 0 {
			continue
		}

		locationKey := fmt.Sprintf("0x%x", devInfo.LocationID)

		var name string
		if devInfo.Name != nil && *devInfo.Name != "" {
			name = *devInfo.Name
		} else {
			name = devInfo.DeviceType
		}

		ecid := strings.ToLower(devInfo.ECID)
		if !strings.HasPrefix(ecid, "0x") {
			ecid = "0x" + ecid
		}

		resolvedMap[locationKey] = ResolvedInfo{
			ECID:       ecid,
			DeviceType: devInfo.DeviceType,
			Name:       name,
		}
	}

	return resolvedMap, nil
}
