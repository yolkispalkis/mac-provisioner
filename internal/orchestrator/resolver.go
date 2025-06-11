package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
type Resolver struct {
	infoLogger  *log.Logger
	debugLogger *log.Logger
}

func NewResolver(infoLogger, debugLogger *log.Logger) *Resolver {
	return &Resolver{
		infoLogger:  infoLogger,
		debugLogger: debugLogger,
	}
}

// GetInfoByECID выполняет `cfgutil list` и возвращает карту [ECID -> Info].
func (r *Resolver) GetInfoByECID(ctx context.Context) (map[string]ResolvedInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if strings.Contains(err.Error(), "exit status 70") {
			return make(map[string]ResolvedInfo), nil
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
		if devInfo.ECID == "" {
			continue
		}

		var name string
		if devInfo.Name != nil && *devInfo.Name != "" {
			name = *devInfo.Name
		} else {
			name = devInfo.DeviceType
		}

		normalizedECID, err := normalizeECID(devInfo.ECID)
		if err != nil {
			r.infoLogger.Printf("⚠️ Не удалось нормализовать ECID от cfgutil: %s", devInfo.ECID)
			continue
		}

		resolvedMap[normalizedECID] = ResolvedInfo{
			ECID:       normalizedECID,
			DeviceType: devInfo.DeviceType,
			Name:       name,
		}
	}

	return resolvedMap, nil
}
