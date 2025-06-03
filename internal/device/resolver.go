package device

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeviceResolver получает имена устройств из cfgutil
type DeviceResolver struct {
	cache     map[string]CachedDevice
	mutex     sync.RWMutex
	debugMode bool
}

type CachedDevice struct {
	Name     string
	CachedAt time.Time
	TTL      time.Duration
}

type CfgutilResponse struct {
	Output map[string]CfgutilDevice `json:"Output"`
}

type CfgutilDevice struct {
	Name       *string `json:"name"`
	DeviceType string  `json:"deviceType"`
}

func NewDeviceResolver() *DeviceResolver {
	return &DeviceResolver{
		cache:     make(map[string]CachedDevice),
		debugMode: os.Getenv("MAC_PROV_DEBUG") == "1",
	}
}

// ResolveDeviceNameSync синхронно получает красивое имя устройства по ECID
func (dr *DeviceResolver) ResolveDeviceNameSync(ctx context.Context, ecid, fallbackName string) string {
	if ecid == "" {
		return fallbackName
	}

	// Проверяем кэш
	dr.mutex.RLock()
	if cached, exists := dr.cache[ecid]; exists {
		if time.Since(cached.CachedAt) < cached.TTL {
			dr.mutex.RUnlock()
			if dr.debugMode {
				log.Printf("🔍 [DEBUG] Имя устройства из кэша: %s -> %s", ecid, cached.Name)
			}
			return cached.Name
		}
	}
	dr.mutex.RUnlock()

	// Получаем из cfgutil синхронно
	name := dr.fetchFromCfgutil(ctx, ecid)
	if name == "" {
		if dr.debugMode {
			log.Printf("🔍 [DEBUG] Не удалось получить имя из cfgutil для ECID %s, используем fallback: %s", ecid, fallbackName)
		}
		return fallbackName
	}

	// Кэшируем результат
	dr.mutex.Lock()
	dr.cache[ecid] = CachedDevice{
		Name:     name,
		CachedAt: time.Now(),
		TTL:      10 * time.Minute,
	}
	dr.mutex.Unlock()

	if dr.debugMode {
		log.Printf("🔍 [DEBUG] Получено имя устройства из cfgutil: %s -> %s", ecid, name)
	}

	return name
}

// ResolveDeviceName для обратной совместимости
func (dr *DeviceResolver) ResolveDeviceName(ctx context.Context, ecid, fallbackName string) string {
	return dr.ResolveDeviceNameSync(ctx, ecid, fallbackName)
}

// fetchFromCfgutil выполняет запрос к cfgutil для получения имени устройства
func (dr *DeviceResolver) fetchFromCfgutil(ctx context.Context, ecid string) string {
	// Создаем контекст с таймаутом для cfgutil
	cfgutilCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cfgutilCtx, "cfgutil", "--format", "JSON", "list")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if dr.debugMode {
			log.Printf("🔍 [DEBUG] Ошибка выполнения cfgutil list: %v, stderr: %s", err, stderr.String())
		}
		return ""
	}

	var response CfgutilResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		if dr.debugMode {
			log.Printf("🔍 [DEBUG] Ошибка парсинга JSON от cfgutil: %v", err)
		}
		return ""
	}

	// Нормализуем ECID для поиска
	normalizedSearchECID := dr.normalizeECIDForSearch(ecid)

	// Ищем устройство по ECID
	for deviceECID, deviceInfo := range response.Output {
		normalizedDeviceECID := dr.normalizeECIDForSearch(deviceECID)

		if normalizedDeviceECID == normalizedSearchECID {
			// Предпочитаем name, если есть, иначе deviceType
			if deviceInfo.Name != nil && *deviceInfo.Name != "" {
				return *deviceInfo.Name
			}
			if deviceInfo.DeviceType != "" {
				return deviceInfo.DeviceType
			}
		}
	}

	return ""
}

// normalizeECIDForSearch нормализует ECID для поиска (приводит к единому формату)
func (dr *DeviceResolver) normalizeECIDForSearch(ecid string) string {
	if ecid == "" {
		return ""
	}

	// Убираем префикс 0x если есть
	clean := strings.TrimPrefix(strings.ToLower(ecid), "0x")

	// Если это hex, конвертируем в decimal для единообразия
	if dr.isHexString(clean) {
		if value, err := strconv.ParseUint(clean, 16, 64); err == nil {
			return strconv.FormatUint(value, 10)
		}
	}

	// Если уже decimal, возвращаем как есть
	if dr.isDecimalString(clean) {
		return clean
	}

	return ecid
}

// isHexString проверяет, является ли строка hex числом
func (dr *DeviceResolver) isHexString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// isDecimalString проверяет, является ли строка decimal числом
func (dr *DeviceResolver) isDecimalString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ClearCache очищает кэш имен устройств
func (dr *DeviceResolver) ClearCache() {
	dr.mutex.Lock()
	defer dr.mutex.Unlock()
	dr.cache = make(map[string]CachedDevice)
	if dr.debugMode {
		log.Printf("🔍 [DEBUG] Кэш имен устройств очищен")
	}
}

// GetCacheStats возвращает статистику кэша
func (dr *DeviceResolver) GetCacheStats() (int, int) {
	dr.mutex.RLock()
	defer dr.mutex.RUnlock()

	total := len(dr.cache)
	valid := 0
	now := time.Now()

	for _, cached := range dr.cache {
		if now.Sub(cached.CachedAt) < cached.TTL {
			valid++
		}
	}

	return total, valid
}

// cleanupExpiredCache периодически очищает устаревшие записи кэша
func (dr *DeviceResolver) StartCleanupRoutine(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			dr.cleanupExpiredCache()
		}
	}
}

func (dr *DeviceResolver) cleanupExpiredCache() {
	dr.mutex.Lock()
	defer dr.mutex.Unlock()

	now := time.Now()
	var expired []string

	for ecid, cached := range dr.cache {
		if now.Sub(cached.CachedAt) >= cached.TTL {
			delete(dr.cache, ecid)
			expired = append(expired, ecid)
		}
	}

	if len(expired) > 0 && dr.debugMode {
		log.Printf("🔍 [DEBUG] Очищены устаревшие записи кэша имен устройств: %d", len(expired))
	}
}
