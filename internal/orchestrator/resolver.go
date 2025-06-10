package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/model"
)

// ResolvedInfo содержит обогащенную информацию об устройстве из cfgutil.
type ResolvedInfo struct {
	ECID       string
	DeviceType string
	Name       string
}

// Resolver обогащает данные об устройствах, используя `cfgutil list`.
type Resolver struct {
	cache      map[string]ResolvedInfo // Ключ - locationID
	cacheTime  time.Time
	cacheTTL   time.Duration
	mu         sync.Mutex
	lastCallOk bool
}

func NewResolver() *Resolver {
	return &Resolver{
		cache:      make(map[string]ResolvedInfo),
		cacheTTL:   10 * time.Second, // Кэшируем результат на 10 секунд
		lastCallOk: true,
	}
}

// ResolveAndUpdate обогащает устройства из списка, используя `cfgutil`.
func (r *Resolver) ResolveAndUpdate(ctx context.Context, devices []*model.Device) {
	r.mu.Lock()
	// Используем кэш, если он свежий
	if time.Since(r.cacheTime) < r.cacheTTL && r.lastCallOk {
		r.mu.Unlock()
		r.updateFromCache(devices)
		return
	}
	r.mu.Unlock()

	// Выполняем `cfgutil list`
	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		r.lastCallOk = false
		log.Printf("⚠️ Не удалось выполнить cfgutil list: %v. Распознавание будет пропущено.", err)
		return
	}
	r.lastCallOk = true

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
		return
	}

	r.mu.Lock()
	r.cache = make(map[string]ResolvedInfo)
	// Сохраняем результат в кэш
	for _, devInfo := range response.Output {
		// system_profiler возвращает locationID в hex (0x...), а cfgutil в decimal.
		// Нам нужен ключ для сопоставления. Удобнее всего - сам ECID.
		if devInfo.ECID == "" {
			continue
		}

		// Нормализуем ECID к нижнему регистру и с префиксом 0x
		ecid := strings.ToLower(devInfo.ECID)
		if !strings.HasPrefix(ecid, "0x") {
			ecid = "0x" + ecid
		}

		var name string
		if devInfo.Name != nil {
			name = *devInfo.Name
		}

		r.cache[ecid] = ResolvedInfo{
			ECID:       ecid,
			DeviceType: devInfo.DeviceType,
			Name:       name,
		}
	}
	r.cacheTime = time.Now()
	r.mu.Unlock()

	r.updateFromCache(devices)
}

func (r *Resolver) updateFromCache(devices []*model.Device) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.cache) == 0 {
		return
	}

	for _, dev := range devices {
		// Если у устройства уже есть ECID, пытаемся его обогатить
		if dev.ECID != "" {
			if info, ok := r.cache[strings.ToLower(dev.ECID)]; ok {
				if info.DeviceType != "" {
					dev.Name = info.DeviceType
				}
				if info.Name != "" {
					dev.Name = info.Name // Более конкретное имя имеет приоритет
				}
			}
		}
	}
}
