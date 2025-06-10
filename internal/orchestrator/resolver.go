package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mac-provisioner/internal/model"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ResolvedInfo содержит обогащенную информацию об устройстве из cfgutil.
type ResolvedInfo struct {
	ECID       string
	DeviceType string
	Name       string
}

// Resolver обогащает данные об устройствах, используя `cfgutil list`.
type Resolver struct {
	cache      map[string]ResolvedInfo // <-- ИЗМЕНЕНИЕ: Ключ - USB Location ID
	cacheTime  time.Time
	cacheTTL   time.Duration
	mu         sync.Mutex
	lastCallOk bool
}

func NewResolver() *Resolver {
	return &Resolver{
		cache:      make(map[string]ResolvedInfo),
		cacheTTL:   5 * time.Second, // Уменьшим TTL, так как состояние портов может меняться
		lastCallOk: true,
	}
}

// ResolveAndUpdate обогащает устройства из списка, используя `cfgutil`.
func (r *Resolver) ResolveAndUpdate(ctx context.Context, devices []*model.Device) {
	r.mu.Lock()
	if time.Since(r.cacheTime) < r.cacheTTL && r.lastCallOk {
		r.mu.Unlock()
		r.updateFromCache(devices)
		return
	}
	r.mu.Unlock()

	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		r.lastCallOk = false
		// Не логируем ошибку, если cfgutil просто ничего не нашел.
		// Это нормальная ситуация, когда нет устройств в DFU/Recovery.
		if !strings.Contains(err.Error(), "exit status 70") {
			log.Printf("⚠️ Не удалось выполнить cfgutil list: %v.", err)
		}
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
	defer r.mu.Unlock()

	r.cache = make(map[string]ResolvedInfo)
	for _, devInfo := range response.Output {
		if devInfo.ECID == "" || devInfo.LocationID == 0 {
			continue
		}

		// --- ИЗМЕНЕНИЕ: Ключом кэша становится USB Location ---
		// system_profiler возвращает locationID в hex (0x...), а cfgutil в decimal.
		// Приводим к формату system_profiler.
		locationKey := fmt.Sprintf("0x%x", devInfo.LocationID)

		ecid := strings.ToLower(devInfo.ECID)
		if !strings.HasPrefix(ecid, "0x") {
			ecid = "0x" + ecid
		}

		var name string
		if devInfo.Name != nil {
			name = *devInfo.Name
		}

		// Сохраняем в кэш по Location ID
		r.cache[locationKey] = ResolvedInfo{
			ECID:       ecid,
			DeviceType: devInfo.DeviceType,
			Name:       name,
		}
	}
	r.cacheTime = time.Now()

	// Сразу же обновляем текущий список устройств
	r.updateDevices(devices)
}

// updateFromCache использует существующий кэш для обновления устройств.
func (r *Resolver) updateFromCache(devices []*model.Device) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateDevices(devices)
}

// updateDevices - внутренняя функция для обогащения списка. Требует внешней блокировки.
func (r *Resolver) updateDevices(devices []*model.Device) {
	if len(r.cache) == 0 {
		return
	}

	for _, dev := range devices {
		if dev.USBLocation == "" {
			continue
		}

		// --- ИЗМЕНЕНИЕ: Ищем устройство в кэше по его USB Location ---
		// system_profiler может возвращать Location ID с под-портом (e.g., "0x01100000 / 1")
		// Нам нужна только базовая часть для сопоставления.
		baseLocation := strings.Split(dev.USBLocation, "/")[0]
		baseLocation = strings.TrimSpace(baseLocation)

		if info, ok := r.cache[baseLocation]; ok {
			// Если у устройства не было ECID, а в кэше он есть - присваиваем.
			if dev.ECID == "" && info.ECID != "" {
				dev.ECID = info.ECID
			}
			// Обновляем имя, так как из cfgutil оно более точное.
			if info.DeviceType != "" {
				dev.Name = info.DeviceType
			}
			if info.Name != "" {
				dev.Name = info.Name // Более конкретное имя имеет приоритет
			}
		}
	}
}
