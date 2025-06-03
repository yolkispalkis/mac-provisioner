package provisioner

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CooldownEntry struct {
	ECID          string
	DeviceName    string
	CompletedAt   time.Time
	CooldownUntil time.Time
}

type CooldownManager struct {
	entries        map[string]*CooldownEntry // key = normalized ECID
	mutex          sync.RWMutex
	cooldownPeriod time.Duration
	debugMode      bool
}

func NewCooldownManager(cooldownPeriod time.Duration) *CooldownManager {
	cm := &CooldownManager{
		entries:        make(map[string]*CooldownEntry),
		cooldownPeriod: cooldownPeriod,
		debugMode:      os.Getenv("MAC_PROV_DEBUG") == "1",
	}

	go cm.cleanupLoop()

	return cm
}

func (cm *CooldownManager) AddCompletedDevice(usbLocation, ecid, deviceName string) {
	if ecid == "" {
		if cm.debugMode {
			log.Printf("🔍 [DEBUG] Не добавляем в кулдаун - нет ECID для %s", deviceName)
		}
		return
	}

	normalizedECID := cm.normalizeECID(ecid)
	if normalizedECID == "" {
		if cm.debugMode {
			log.Printf("🔍 [DEBUG] Не удалось нормализовать ECID %s для %s", ecid, deviceName)
		}
		return
	}

	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	now := time.Now()
	entry := &CooldownEntry{
		ECID:          normalizedECID,
		DeviceName:    deviceName,
		CompletedAt:   now,
		CooldownUntil: now.Add(cm.cooldownPeriod),
	}

	cm.entries[normalizedECID] = entry

	log.Printf("🕒 %s добавлен в период охлаждения до %s",
		deviceName, entry.CooldownUntil.Format("15:04"))
}

// IsDeviceInCooldown проверяет, находится ли конкретное устройство в кулдауне
func (cm *CooldownManager) IsDeviceInCooldown(ecid string) (bool, time.Duration, string) {
	if ecid == "" {
		return false, 0, ""
	}

	normalizedECID := cm.normalizeECID(ecid)
	if normalizedECID == "" {
		return false, 0, ""
	}

	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entry, exists := cm.entries[normalizedECID]
	if !exists {
		return false, 0, ""
	}

	now := time.Now()
	if now.Before(entry.CooldownUntil) {
		remaining := entry.CooldownUntil.Sub(now)
		return true, remaining, entry.DeviceName
	}

	return false, 0, ""
}

// ShouldTriggerDFU определяет, нужно ли запускать DFU для порта
// Логика:
// - Если порт пустой -> запускать DFU каждые 3 секунды
// - Если есть устройство и оно НЕ в кулдауне -> запускать DFU
// - Если есть устройство и оно в кулдауне -> НЕ запускать DFU
func (cm *CooldownManager) ShouldTriggerDFU(deviceECID string) (bool, string) {
	// Если устройства нет (порт пустой) - всегда разрешаем DFU
	if deviceECID == "" {
		return true, "порт пустой"
	}

	// Проверяем, в кулдауне ли устройство
	inCooldown, remaining, deviceName := cm.IsDeviceInCooldown(deviceECID)
	if inCooldown {
		reason := fmt.Sprintf("устройство %s в кулдауне (осталось %v)",
			deviceName, remaining.Round(time.Minute))
		return false, reason
	}

	// Устройство есть, но не в кулдауне - разрешаем DFU
	return true, "устройство не в кулдауне"
}

func (cm *CooldownManager) RemoveCooldown(ecid string) {
	if ecid == "" {
		return
	}

	normalizedECID := cm.normalizeECID(ecid)
	if normalizedECID == "" {
		return
	}

	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if entry, exists := cm.entries[normalizedECID]; exists {
		delete(cm.entries, normalizedECID)
		log.Printf("🕒 Период охлаждения снят для %s", entry.DeviceName)
	}
}

func (cm *CooldownManager) GetAllCooldowns() []*CooldownEntry {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	var active []*CooldownEntry
	now := time.Now()

	for _, entry := range cm.entries {
		if now.Before(entry.CooldownUntil) {
			entryCopy := *entry
			active = append(active, &entryCopy)
		}
	}

	return active
}

// normalizeECID нормализует ECID для единообразного сравнения
func (cm *CooldownManager) normalizeECID(ecid string) string {
	if ecid == "" {
		return ""
	}

	// Убираем префикс 0x и приводим к нижнему регистру
	clean := strings.ToLower(strings.TrimPrefix(ecid, "0x"))

	// Если это hex, конвертируем в decimal для единообразия
	if cm.isHexString(clean) {
		if value, err := strconv.ParseUint(clean, 16, 64); err == nil {
			return strconv.FormatUint(value, 10)
		}
	}

	// Если уже decimal, возвращаем как есть
	if cm.isDecimalString(clean) {
		return clean
	}

	return clean
}

func (cm *CooldownManager) isHexString(s string) bool {
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

func (cm *CooldownManager) isDecimalString(s string) bool {
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

func (cm *CooldownManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cm.cleanup()
	}
}

func (cm *CooldownManager) cleanup() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	now := time.Now()
	var removed []string

	for ecid, entry := range cm.entries {
		if now.After(entry.CooldownUntil) {
			delete(cm.entries, ecid)
			removed = append(removed, entry.DeviceName)
		}
	}

	if len(removed) > 0 && cm.debugMode {
		log.Printf("🔍 [DEBUG] Очищены периоды охлаждения для: %v", removed)
	}
}
