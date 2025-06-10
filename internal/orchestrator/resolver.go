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
	cache      map[string]ResolvedInfo // Ключ - USB Location ID
	cacheTime  time.Time
	cacheTTL   time.Duration
	mu         sync.Mutex
	lastCallOk bool
}

func NewResolver() *Resolver {
	return &Resolver{
		cache:      make(map[string]ResolvedInfo),
		cacheTTL:   10 * time.Second, // Можно вернуть обратно на 10с
		lastCallOk: true,
	}
}

// ResolveAndUpdate обогащает устройства и возвращает карту распознанных устройств.
func (r *Resolver) ResolveAndUpdate(ctx context.Context, devices []*model.Device) map[string]ResolvedInfo {
	r.mu.Lock()
	if time.Since(r.cacheTime) < r.cacheTTL && r.lastCallOk {
		r.mu.Unlock()
		return r.updateFromCache(devices)
	}
	r.mu.Unlock()

	cmd := exec.CommandContext(ctx, "cfgutil", "--format", "json", "list")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		r.lastCallOk = false
		if !strings.Contains(err.Error(), "exit status 70") {
			log.Printf("⚠️ Не удалось выполнить cfgutil list: %v.", err)
		}
		return nil
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
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cache = make(map[string]ResolvedInfo)
	newlyResolved := make(map[string]ResolvedInfo)

	for _, devInfo := range response.Output {
		if devInfo.ECID == "" {
			continue
		}

		locationKey := fmt.Sprintf("0x%x", devInfo.LocationID)
		ecid := strings.ToLower(devInfo.ECID)
		if !strings.HasPrefix(ecid, "0x") {
			ecid = "0x" + ecid
		}

		var name string
		if devInfo.Name != nil && *devInfo.Name != "" {
			name = *devInfo.Name
		} else {
			name = devInfo.DeviceType
		}

		info := ResolvedInfo{
			ECID:       ecid,
			DeviceType: devInfo.DeviceType,
			Name:       name,
		}

		if locationKey != "0x0" {
			r.cache[locationKey] = info
		}
		newlyResolved[ecid] = info
	}
	r.cacheTime = time.Now()

	r.updateDevices(devices)
	return newlyResolved
}

func (r *Resolver) updateFromCache(devices []*model.Device) map[string]ResolvedInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateDevices(devices)

	// Возвращаем копию кэша, преобразованную для оркестратора
	resolvedMap := make(map[string]ResolvedInfo)
	for _, info := range r.cache {
		resolvedMap[info.ECID] = info
	}
	return resolvedMap
}

func (r *Resolver) updateDevices(devices []*model.Device) {
	if len(r.cache) == 0 {
		return
	}

	for _, dev := range devices {
		if dev.USBLocation == "" {
			continue
		}
		baseLocation := strings.TrimSpace(strings.Split(dev.USBLocation, "/")[0])
		if info, ok := r.cache[baseLocation]; ok {
			if dev.ECID == "" && info.ECID != "" {
				dev.ECID = info.ECID
			}
			if info.Name != "" {
				dev.Name = info.Name
			}
		}
	}
}
