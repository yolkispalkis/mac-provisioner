package device

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mac-provisioner/internal/config"
)

/*
----------------------------------------------------------------------
|  СОБЫТИЯ
----------------------------------------------------------------------
*/
const (
	EventConnected    = "connected"
	EventDisconnected = "disconnected"
	EventStateChanged = "state_changed"
)

type Event struct {
	Type   string  `json:"type"`
	Device *Device `json:"device"`
}

/*
----------------------------------------------------------------------
|  MONITOR STRUCT
----------------------------------------------------------------------
*/
type Monitor struct {
	config       config.MonitoringConfig
	eventChan    chan Event
	devices      map[string]*Device
	devicesMutex sync.RWMutex
	running      bool
	ctx          context.Context
	cancel       context.CancelFunc
	firstScan    bool
}

func NewMonitor(cfg config.MonitoringConfig) *Monitor {
	return &Monitor{
		config:    cfg,
		eventChan: make(chan Event, cfg.EventBufferSize),
		devices:   make(map[string]*Device),
		firstScan: true,
	}
}

/*
----------------------------------------------------------------------
|  START / STOP
----------------------------------------------------------------------
*/
func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}
	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB устройств...")

	if err := m.checkCfgutilAvailable(); err != nil {
		log.Printf("⚠️ %v", err)
	}

	if err := m.initialScan(); err != nil {
		log.Printf("⚠️ начальное сканирование: %v", err)
	}

	go m.monitorLoop()
	go m.cleanupLoop()

	log.Println("✅ Мониторинг USB устройств запущен")
	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}
	log.Println("🛑 Остановка мониторинга USB...")
	m.running = false
	if m.cancel != nil {
		m.cancel()
	}
	close(m.eventChan)
}

func (m *Monitor) Events() <-chan Event { return m.eventChan }

/*
----------------------------------------------------------------------
|  cfgutil PATH helper
----------------------------------------------------------------------
*/
func (m *Monitor) checkCfgutilAvailable() error {
	if _, err := exec.LookPath("cfgutil"); err == nil {
		log.Println("✅ cfgutil доступен (найден в $PATH)")
		return nil
	}
	def := "/Applications/Apple Configurator.app/Contents/MacOS/cfgutil"
	if _, err := os.Stat(def); err == nil {
		dir := filepath.Dir(def)
		if !strings.Contains(os.Getenv("PATH"), dir) {
			_ = os.Setenv("PATH", os.Getenv("PATH")+":"+dir)
		}
		log.Printf("ℹ️  cfgutil найден по пути %s — директория добавлена в $PATH", def)
		return nil
	}
	return fmt.Errorf("cfgutil недоступен. Установите Apple Configurator 2")
}

/*
----------------------------------------------------------------------
|  MAIN LOOPS
----------------------------------------------------------------------
*/
func (m *Monitor) monitorLoop() {
	t := time.NewTicker(m.config.CheckInterval)
	defer t.Stop()
	log.Printf("🔄 Цикл мониторинга %v", m.config.CheckInterval)

	for {
		select {
		case <-m.ctx.Done():
			log.Println("🛑 Цикл мониторинга остановлен")
			return
		case <-t.C:
			m.checkDevices()
		}
	}
}

func (m *Monitor) cleanupLoop() {
	t := time.NewTicker(m.config.CleanupInterval)
	defer t.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.cleanup()
		}
	}
}

/*
----------------------------------------------------------------------
|  CHECK DEVICES
----------------------------------------------------------------------
*/
func (m *Monitor) checkDevices() {
	current := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	currMap := make(map[string]*Device, len(current))
	for _, d := range current {
		if d.IsValidSerial() {
			currMap[d.SerialNumber] = d
		}
	}

	if m.firstScan {
		log.Println("🔍 Первое сканирование — генерируем события")
		for sn, d := range currMap {
			m.devices[sn] = d
			m.sendEvent(Event{Type: EventConnected, Device: d})
		}
		m.firstScan = false
		return
	}

	/* новые / изменённые */
	for sn, d := range currMap {
		if old, ok := m.devices[sn]; !ok {
			m.devices[sn] = d
			m.sendEvent(Event{Type: EventConnected, Device: d})
		} else if old.State != d.State || old.IsDFU != d.IsDFU {
			m.devices[sn] = d
			m.sendEvent(Event{Type: EventStateChanged, Device: d})
		}
	}

	/* отключённые */
	for sn, d := range m.devices {
		if _, ok := currMap[sn]; !ok {
			delete(m.devices, sn)
			m.sendEvent(Event{Type: EventDisconnected, Device: d})
		}
	}
}

/*
----------------------------------------------------------------------
|  COLLECT DEVICES
----------------------------------------------------------------------
*/
func (m *Monitor) getCurrentDevices() []*Device {
	var res []*Device
	res = append(res, m.getCfgutilDevices()...)
	res = append(res, m.getDFUDevices()...)
	return m.removeDuplicates(res)
}

func (m *Monitor) cfgutilCmd(args ...string) *exec.Cmd {
	return exec.Command("cfgutil", args...)
}

/*------------------- обычные устройства -------------------*/
func (m *Monitor) getCfgutilDevices() []*Device {
	out, err := m.cfgutilCmd("list").Output()
	if err != nil {
		log.Printf("❌ cfgutil list: %v", err)
		return nil
	}
	return m.parseCfgutilOutput(string(out))
}

/*------------------- DFU / Recovery / Restore -------------*/
func (m *Monitor) getDFUDevices() []*Device {
	out, err := m.cfgutilCmd("list").Output()
	if err != nil {
		return nil
	}
	return m.parseDFULines(string(out))
}

/*
----------------------------------------------------------------------
|  PARSERS
----------------------------------------------------------------------
*/
func (m *Monitor) parseCfgutilOutput(out string) []*Device {
	var list []*Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" ||
			strings.HasPrefix(line, "Name") ||
			strings.HasPrefix(line, "ECID") ||
			(strings.HasPrefix(line, "Type:") && strings.Contains(line, "ECID:")) {
			continue
		}
		if d := m.parseDeviceLine(line); d != nil && !d.IsDFU {
			list = append(list, d)
		}
	}
	return list
}

func (m *Monitor) parseDFULines(out string) []*Device {
	var list []*Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lc := strings.ToLower(line)
		if !(strings.Contains(lc, "dfu") || strings.Contains(lc, "recovery") ||
			strings.Contains(lc, "restore")) {
			continue // строка про «живой» Mac, пропускаем
		}
		if !strings.Contains(line, "Type:") || !strings.Contains(line, "ECID:") {
			continue // нет ключевых полей
		}
		if d := m.parseDFULine(line); d != nil {
			list = append(list, d)
		}
	}
	return list
}

/*--- device line (оба формата) ---*/
func (m *Monitor) parseDeviceLine(line string) *Device {
	d := &Device{}

	/*----- новый формат (табами) -----*/
	if strings.Contains(line, "\t") {
		p := strings.Split(line, "\t")
		if len(p) >= 3 {
			d.SerialNumber = strings.TrimSpace(p[0])
			d.Model = strings.TrimSpace(p[1])
			d.State = strings.TrimSpace(p[2])

			if len(p) > 3 {
				for _, x := range p[3:] {
					if strings.Contains(strings.ToLower(x), "location") {
						d.USBLocation = strings.TrimSpace(x)
						break
					}
				}
			}
		}
	} else {
		/*----- старый формат -----*/
		fields := strings.Fields(line)
		if len(fields) < 1 {
			return nil
		}
		d.SerialNumber = fields[0]

		if s := strings.Index(line, "("); s != -1 {
			if e := strings.Index(line[s:], ")"); e != -1 {
				d.Model = line[s+1 : s+e]
			}
		}

		if idx := strings.Index(line, "Location:"); idx != -1 {
			parts := strings.Fields(line[idx:])
			if len(parts) >= 2 {
				d.USBLocation = parts[1]
			}
		}

		if dash := strings.LastIndex(line, " - "); dash != -1 {
			d.State = strings.TrimSpace(line[dash+3:])
		} else {
			d.State = "Unknown"
		}
	}

	// определяем DFU-подобное состояние
	stateLower := strings.ToLower(d.State)
	d.IsDFU = strings.Contains(stateLower, "dfu") ||
		strings.Contains(stateLower, "recovery") ||
		strings.Contains(stateLower, "restore")

	return d
}

/*--- DFU line ---*/
func (m *Monitor) parseDFULine(line string) *Device {
	lc := strings.ToLower(line)
	if !(strings.Contains(lc, "dfu") || strings.Contains(lc, "recovery") ||
		strings.Contains(lc, "restore")) {
		return nil
	}

	d := &Device{IsDFU: true, State: "DFU"}
	parts := strings.Fields(line)
	for i, p := range parts {
		switch p {
		case "Type:":
			if i+1 < len(parts) {
				d.Model = parts[i+1]
			}
		case "ECID:":
			if i+1 < len(parts) {
				d.ECID = parts[i+1]
				d.SerialNumber = "DFU-" + parts[i+1]
			}
		case "Location:":
			if i+1 < len(parts) {
				d.USBLocation = parts[i+1]
			}
		}
	}
	return d
}

/*
----------------------------------------------------------------------
|  HELPERS
----------------------------------------------------------------------
*/
func (m *Monitor) removeDuplicates(list []*Device) []*Device {
	seen := map[string]bool{}
	var out []*Device
	for _, d := range list {
		if d.SerialNumber != "" && !seen[d.SerialNumber] {
			seen[d.SerialNumber] = true
			out = append(out, d)
		}
	}
	return out
}

func (m *Monitor) sendEvent(e Event) {
	select {
	case m.eventChan <- e:
		log.Printf("📤 Событие: %s -> %s", e.Type, e.Device.SerialNumber)
	default:
		log.Println("⚠️ Буфер событий переполнен — событие пропущено")
	}
}

/*
----------------------------------------------------------------------
|  SERVICE ROUTINES
----------------------------------------------------------------------
*/
func (m *Monitor) initialScan() error {
	log.Println("🔍 Начальное сканирование…")
	devs := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	for _, d := range devs {
		if d.IsValidSerial() {
			log.Printf("📱 Найдено при запуске: %s (%s) - %s", d.SerialNumber, d.Model, d.State)
		}
	}
	log.Printf("✅ Начальное сканирование: %d устройств", len(devs))
	return nil
}

func (m *Monitor) cleanup() {
	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()
	log.Printf("🧹 Очистка: отслеживается %d устройств", len(m.devices))
}

func (m *Monitor) GetConnectedDevices() []*Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	out := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out
}
