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

/* ============================================================
   СОБЫТИЯ
   ============================================================ */

const (
	EventConnected    = "connected"
	EventDisconnected = "disconnected"
	EventStateChanged = "state_changed"
)

type Event struct {
	Type   string  `json:"type"`
	Device *Device `json:"device"`
}

/* ============================================================
   MONITOR
   ============================================================ */

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

/* -------------------- START / STOP -------------------- */

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("монитор уже запущен")
	}

	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("🔍 Запуск мониторинга USB устройств...")

	// Проверяем наличие cfgutil (и при необходимости добавляем в PATH)
	if err := m.checkCfgutilAvailable(); err != nil {
		log.Printf("⚠️ Предупреждение: %v", err)
	}

	// Первое сканирование
	if err := m.initialScan(); err != nil {
		log.Printf("⚠️ Предупреждение: ошибка начального сканирования: %v", err)
	}

	// Фоновые циклы
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

/* -------------------- cfgutil helper -------------------- */

func (m *Monitor) checkCfgutilAvailable() error {
	// Есть ли в PATH?
	if _, err := exec.LookPath("cfgutil"); err == nil {
		log.Println("✅ cfgutil доступен (найден в $PATH)")
		return nil
	}

	// Штатный путь от Apple Configurator
	defPath := "/Applications/Apple Configurator.app/Contents/MacOS/cfgutil"
	if _, err := os.Stat(defPath); err == nil {
		dir := filepath.Dir(defPath)
		if !strings.Contains(os.Getenv("PATH"), dir) {
			_ = os.Setenv("PATH", os.Getenv("PATH")+":"+dir)
		}
		log.Printf("ℹ️  cfgutil найден по пути %s — директория добавлена в $PATH", defPath)
		return nil
	}

	return fmt.Errorf("cfgutil недоступен. Установите Apple Configurator 2")
}

/* ============================================================
   ЦИКЛЫ
   ============================================================ */

func (m *Monitor) monitorLoop() {
	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	log.Printf("🔄 Запуск цикла мониторинга с интервалом %v", m.config.CheckInterval)

	for {
		select {
		case <-m.ctx.Done():
			log.Println("🛑 Цикл мониторинга остановлен")
			return
		case <-ticker.C:
			m.checkDevices()
		}
	}
}

func (m *Monitor) cleanupLoop() {
	ticker := time.NewTicker(m.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.cleanup()
		}
	}
}

/* ============================================================
   ОБРАБОТКА УСТРОЙСТВ
   ============================================================ */

func (m *Monitor) checkDevices() {
	currentDevices := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	/* --- формируем map текущих --- */
	currentMap := make(map[string]*Device)
	for _, dev := range currentDevices {
		if dev.IsValidSerial() {
			currentMap[dev.SerialNumber] = dev
		}
	}

	/* --- первое сканирование --- */
	if m.firstScan {
		log.Println("🔍 Первое сканирование — генерируем события для всех обнаруженных устройств")
		for sn, dev := range currentMap {
			m.devices[sn] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		}
		m.firstScan = false
		return
	}

	/* --- новые устройства --- */
	for sn, dev := range currentMap {
		if old, ok := m.devices[sn]; !ok {
			m.devices[sn] = dev
			m.sendEvent(Event{Type: EventConnected, Device: dev})
		} else if old.State != dev.State || old.IsDFU != dev.IsDFU {
			m.devices[sn] = dev
			m.sendEvent(Event{Type: EventStateChanged, Device: dev})
		}
	}

	/* --- отключённые устройства --- */
	for sn, dev := range m.devices {
		if _, ok := currentMap[sn]; !ok {
			delete(m.devices, sn)
			m.sendEvent(Event{Type: EventDisconnected, Device: dev})
		}
	}
}

/* -------------------- Получаем список устройств -------------------- */

func (m *Monitor) getCurrentDevices() []*Device {
	var res []*Device
	res = append(res, m.getCfgutilDevices()...)
	res = append(res, m.getDFUDevices()...)
	return m.removeDuplicates(res)
}

func (m *Monitor) cfgutilCmd(args ...string) *exec.Cmd {
	return exec.Command("cfgutil", args...)
}

/* --- обычные устройства --- */

func (m *Monitor) getCfgutilDevices() []*Device {
	out, err := m.cfgutilCmd("list").Output()
	if err != nil {
		log.Printf("❌ cfgutil list: %v", err)
		return nil
	}
	return m.parseCfgutilOutput(string(out))
}

/* --- DFU устройства (строки с Type:/ECID:) --- */

func (m *Monitor) getDFUDevices() []*Device {
	out, err := m.cfgutilCmd("list").Output()
	if err != nil {
		return nil
	}
	return m.parseDFUOutput(string(out))
}

/* -------------------- парсинг вывода cfgutil -------------------- */

func (m *Monitor) parseCfgutilOutput(out string) []*Device {
	var devs []*Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" ||
			strings.HasPrefix(line, "ECID") ||
			strings.HasPrefix(line, "Name") ||
			(strings.HasPrefix(line, "Type:") && strings.Contains(line, "ECID:")) {
			continue
		}
		if d := m.parseDeviceLine(line); d != nil && !d.IsDFU {
			devs = append(devs, d)
		}
	}
	return devs
}

func (m *Monitor) parseDFUOutput(out string) []*Device {
	var devs []*Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Type:") && strings.Contains(line, "ECID:") {
			if d := m.parseDFULine(line); d != nil {
				devs = append(devs, d)
			}
		}
	}
	return devs
}

/* --- парсер строк (оба формата) --- */

func (m *Monitor) parseDeviceLine(line string) *Device {
	d := &Device{}

	/* новый формат (с табами) */
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
			s := strings.ToLower(d.State)
			d.IsDFU = strings.Contains(s, "dfu") || strings.Contains(s, "recovery")
			return d
		}
	}

	/* старый формат */
	fields := strings.Fields(line)
	if len(fields) < 1 {
		return nil
	}
	d.SerialNumber = fields[0]

	if start := strings.Index(line, "("); start != -1 {
		if end := strings.Index(line[start:], ")"); end != -1 {
			d.Model = line[start+1 : start+end]
		}
	}

	if idx := strings.Index(line, "Location:"); idx != -1 {
		locFields := strings.Fields(line[idx:])
		if len(locFields) >= 2 {
			d.USBLocation = locFields[1]
		}
	}

	if dash := strings.LastIndex(line, " - "); dash != -1 {
		d.State = strings.TrimSpace(line[dash+3:])
	} else {
		d.State = "Unknown"
	}
	s := strings.ToLower(d.State)
	d.IsDFU = strings.Contains(s, "dfu") || strings.Contains(s, "recovery")
	return d
}

func (m *Monitor) parseDFULine(line string) *Device {
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

/* -------------------- утилиты -------------------- */

func (m *Monitor) removeDuplicates(list []*Device) []*Device {
	seen := map[string]bool{}
	var res []*Device
	for _, d := range list {
		if d.SerialNumber != "" && !seen[d.SerialNumber] {
			seen[d.SerialNumber] = true
			res = append(res, d)
		}
	}
	return res
}

func (m *Monitor) sendEvent(e Event) {
	select {
	case m.eventChan <- e:
		log.Printf("📤 Отправлено событие: %s для устройства %s", e.Type, e.Device.SerialNumber)
	default:
		log.Println("⚠️ Буфер событий переполнен, событие пропущено")
	}
}

/* -------------------- сервисные -------------------- */

func (m *Monitor) initialScan() error {
	log.Println("🔍 Выполнение начального сканирования…")
	devs := m.getCurrentDevices()

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	for _, d := range devs {
		if d.IsValidSerial() {
			log.Printf("📱 Найдено при начальном сканировании: %s (%s) - %s",
				d.SerialNumber, d.Model, d.State)
		}
	}
	log.Printf("✅ Начальное сканирование завершено: найдено %d устройств", len(devs))
	return nil
}

func (m *Monitor) cleanup() {
	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()
	log.Printf("🧹 Очистка: отслеживается %d устройств", len(m.devices))
}

/* -------------------- API для отладки -------------------- */

func (m *Monitor) GetConnectedDevices() []*Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	out := make([]*Device, 0, len(m.devices))
	for _, d := range m.devices {
		out = append(out, d)
	}
	return out
}
