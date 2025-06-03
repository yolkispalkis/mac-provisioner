package provisioner

import (
	"log"
	"os"
	"sync"
	"time"
)

type CooldownEntry struct {
	USBLocation   string
	ECID          string
	DeviceName    string
	CompletedAt   time.Time
	CooldownUntil time.Time
}

type CooldownManager struct {
	entries        map[string]*CooldownEntry
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
	if usbLocation == "" {
		return
	}

	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	now := time.Now()
	entry := &CooldownEntry{
		USBLocation:   usbLocation,
		ECID:          ecid,
		DeviceName:    deviceName,
		CompletedAt:   now,
		CooldownUntil: now.Add(cm.cooldownPeriod),
	}

	cm.entries[usbLocation] = entry

	log.Printf("🕒 %s добавлен в период охлаждения до %s",
		deviceName, entry.CooldownUntil.Format("15:04"))
}

func (cm *CooldownManager) IsInCooldown(usbLocation string) (bool, *CooldownEntry) {
	if usbLocation == "" {
		return false, nil
	}

	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	entry, exists := cm.entries[usbLocation]
	if !exists {
		return false, nil
	}

	if time.Now().Before(entry.CooldownUntil) {
		return true, entry
	}

	return false, nil
}

func (cm *CooldownManager) GetCooldownInfo(usbLocation string) (bool, time.Duration, string) {
	inCooldown, entry := cm.IsInCooldown(usbLocation)
	if !inCooldown {
		return false, 0, ""
	}

	remaining := time.Until(entry.CooldownUntil)
	return true, remaining, entry.DeviceName
}

func (cm *CooldownManager) RemoveCooldown(usbLocation string) {
	if usbLocation == "" {
		return
	}

	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if entry, exists := cm.entries[usbLocation]; exists {
		delete(cm.entries, usbLocation)
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

	for location, entry := range cm.entries {
		if now.After(entry.CooldownUntil) {
			delete(cm.entries, location)
			removed = append(removed, entry.DeviceName)
		}
	}

	if len(removed) > 0 && cm.debugMode {
		log.Printf("🔍 [DEBUG] Очищены периоды охлаждения для: %v", removed)
	}
}
