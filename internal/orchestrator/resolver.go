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
)

// Resolver вызывает `cfgutil list` для получения точных имен устройств.
type Resolver struct {
	cache      map[string]string // Кэш [ECID -> Name]
	cacheTime  time.Time
	cacheTTL   time.Duration
	mu         sync.Mutex
	lastCallOk bool
}

func NewResolver() *Resolver {
	return &Resolver{
		cache:      make(map[string]string),
		cacheTTL:   10 * time.Second,
		lastCallOk: true,
	}
}

// GetResolvedNames выполняет `cfgutil list` и возвращает карту [ECID -> Name].
func (r *Resolver) GetResolvedNames(ctx context.Context) map[string]string {
	r.mu.Lock()
	// Используем кэш, если он свежий и последняя попытка была успешной
	if time.Since(r.cacheTime) < r.cacheTTL && r.lastCallOk {
		defer r.mu.Unlock()
		// Возвращаем копию, чтобы избежать гонки данных
		cacheCopy := make(map[string]string, len(r.cache))
		for k, v := range r.cache {
			cacheCopy[k] = v
		}
		return cacheCopy
	}
	r.mu.Unlock()

	// Выполняем `cfgutil list`
	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		r.lastCallOk = false
		if !strings.Contains(err.Error(), "exit status 70") { // Игнорируем ошибку "нет устройств"
			log.Printf("⚠️ Не удалось выполнить cfgutil list: %v.", err)
		}
		return nil
	}
	r.lastCallOk = true

	var response struct {
		Output map[string]struct {
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

	r.cache = make(map[string]string) // Очищаем старый кэш

	for _, devInfo := range response.Output {
		if devInfo.ECID == "" {
			continue
		}

		ecid := strings.ToLower(devInfo.ECID)
		if !strings.HasPrefix(ecid, "0x") {
			ecid = "0x" + ecid
		}

		var name string
		if devInfo.Name != nil && *devInfo.Name != "" {
			name = *devInfo.Name // Приоритет у кастомного имени
		} else {
			name = devInfo.DeviceType // Иначе используем тип устройства
		}

		if name != "" {
			r.cache[ecid] = name
		}
	}
	r.cacheTime = time.Now()

	// Возвращаем копию нового кэша
	cacheCopy := make(map[string]string, len(r.cache))
	for k, v := range r.cache {
		cacheCopy[k] = v
	}
	return cacheCopy
}
