package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ResolvedDevice содержит информацию, полученную из cfgutil.
type ResolvedDevice struct {
	ECID       string
	DeviceType string
	Name       string
	LocationID string // Location ID в формате 0x...
}

// Resolver вызывает `cfgutil list`.
type Resolver struct {
	cache     map[string]*ResolvedDevice // Кэш [LocationID -> ResolvedDevice]
	cacheTime time.Time
	cacheTTL  time.Duration
	mu        sync.Mutex
}

func NewResolver() *Resolver {
	return &Resolver{
		cache:    make(map[string]*ResolvedDevice),
		cacheTTL: 5 * time.Second, // Короткий TTL, так как состояние портов важно
	}
}

// GetResolvedDevices выполняет `cfgutil list` и возвращает карту устройств.
func (r *Resolver) GetResolvedDevices(ctx context.Context) map[string]*ResolvedDevice {
	r.mu.Lock()
	if time.Since(r.cacheTime) < r.cacheTTL {
		defer r.mu.Unlock()
		return r.copyCache()
	}
	r.mu.Unlock()

	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// Ошибка "нет устройств" (код 70) - это не ошибка приложения, а нормальная ситуация.
		if !strings.Contains(err.Error(), "exit status 70") {
			log.Printf("⚠️ Не удалось выполнить cfgutil list: %v.", err)
		}
		return nil
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
		log.Printf("⚠️ Не удалось разобрать JSON от cfgutil: %v", err)
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache = make(map[string]*ResolvedDevice) // Очищаем старый кэш

	for _, devInfo := range response.Output {
		if devInfo.LocationID == 0 {
			continue
		}

		// Приводим locationID к формату system_profiler (hex)
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

		r.cache[locationKey] = &ResolvedDevice{
			ECID:       ecid,
			DeviceType: devInfo.DeviceType,
			Name:       name,
			LocationID: locationKey,
		}
	}
	r.cacheTime = time.Now()

	return r.copyCache()
}

func (r *Resolver) copyCache() map[string]*ResolvedDevice {
	cacheCopy := make(map[string]*ResolvedDevice, len(r.cache))
	for k, v := range r.cache {
		cacheCopy[k] = v
	}
	return cacheCopy
}
