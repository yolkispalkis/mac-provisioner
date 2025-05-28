package stats

import (
	"fmt"
	"sync"
	"time"
)

type Statistics struct {
	mu               sync.RWMutex
	devicesStarted   int
	devicesCompleted int
	devicesFailed    int
	totalProcessTime time.Duration
	startTime        time.Time
}

func NewStatistics() *Statistics {
	return &Statistics{
		startTime: time.Now(),
	}
}

func (s *Statistics) DeviceStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devicesStarted++
}

func (s *Statistics) DeviceCompleted(success bool, processTime time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		s.devicesCompleted++
	} else {
		s.devicesFailed++
	}

	s.totalProcessTime += processTime
}

func (s *Statistics) GetSummary() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	uptime := time.Since(s.startTime)
	avgProcessTime := time.Duration(0)

	if s.devicesCompleted+s.devicesFailed > 0 {
		avgProcessTime = s.totalProcessTime / time.Duration(s.devicesCompleted+s.devicesFailed)
	}

	return fmt.Sprintf("Uptime: %v, Started: %d, Completed: %d, Failed: %d, Avg Process Time: %v",
		uptime.Round(time.Second), s.devicesStarted, s.devicesCompleted, s.devicesFailed, avgProcessTime.Round(time.Second))
}

func (s *Statistics) GetStats() (int, int, int, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.devicesStarted, s.devicesCompleted, s.devicesFailed, time.Since(s.startTime)
}
