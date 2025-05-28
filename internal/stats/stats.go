package stats

import (
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu               sync.RWMutex
	devicesStarted   int
	devicesCompleted int
	devicesFailed    int
	totalProcessTime time.Duration
	startTime        time.Time
}

func New() *Manager {
	return &Manager{
		startTime: time.Now(),
	}
}

func (s *Manager) DeviceStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devicesStarted++
}

func (s *Manager) DeviceCompleted(success bool, processTime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		s.devicesCompleted++
	} else {
		s.devicesFailed++
	}

	s.totalProcessTime += processTime
}

func (s *Manager) Summary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	uptime := time.Since(s.startTime)
	avgProcessTime := time.Duration(0)

	if s.devicesCompleted+s.devicesFailed > 0 {
		avgProcessTime = s.totalProcessTime / time.Duration(s.devicesCompleted+s.devicesFailed)
	}

	return fmt.Sprintf("Время работы: %v, Начато: %d, Завершено: %d, Ошибок: %d, Среднее время: %v",
		uptime.Round(time.Second), s.devicesStarted, s.devicesCompleted, s.devicesFailed, avgProcessTime.Round(time.Second))
}

func (s *Manager) GetStats() (int, int, int, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devicesStarted, s.devicesCompleted, s.devicesFailed, time.Since(s.startTime)
}

func (s *Manager) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.devicesStarted = 0
	s.devicesCompleted = 0
	s.devicesFailed = 0
	s.totalProcessTime = 0
	s.startTime = time.Now()
}
