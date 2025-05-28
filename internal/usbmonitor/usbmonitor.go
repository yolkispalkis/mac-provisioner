package usbmonitor

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unsafe"

	"mac-provisioner/internal/device"
)

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/IOKitLib.h>
#include <IOKit/usb/IOUSBLib.h>
#include <CoreFoundation/CoreFoundation.h>

typedef struct {
    IONotificationPortRef notificationPort;
    io_iterator_t addedIter;
    io_iterator_t removedIter;
    CFRunLoopRef runLoop;
} NotificationData;

extern void deviceAddedCallback(void *refCon, io_iterator_t iterator);
extern void deviceRemovedCallback(void *refCon, io_iterator_t iterator);

int setupUSBNotifications(NotificationData *data, void *refCon) {
    kern_return_t kr;
    CFMutableDictionaryRef matchingDict;

    // Создаем notification port
    data->notificationPort = IONotificationPortCreate(kIOMasterPortDefault);
    if (!data->notificationPort) {
        return -1;
    }

    // Получаем run loop source
    CFRunLoopSourceRef runLoopSource = IONotificationPortGetRunLoopSource(data->notificationPort);
    data->runLoop = CFRunLoopGetCurrent();
    CFRunLoopAddSource(data->runLoop, runLoopSource, kCFRunLoopDefaultMode);

    // Создаем matching dictionary для USB устройств Apple (Vendor ID 0x05AC)
    matchingDict = IOServiceMatching(kIOUSBDeviceClassName);
    if (!matchingDict) {
        return -2;
    }

    // Добавляем Vendor ID для Apple
    CFNumberRef vendorID = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &(int){0x05AC});
    CFDictionarySetValue(matchingDict, CFSTR(kUSBVendorID), vendorID);
    CFRelease(vendorID);

    // Регистрируем callback для подключения устройств
    kr = IOServiceAddMatchingNotification(
        data->notificationPort,
        kIOFirstMatchNotification,
        matchingDict,
        deviceAddedCallback,
        refCon,
        &data->addedIter
    );

    if (kr != KERN_SUCCESS) {
        return -3;
    }

    // Обрабатываем уже подключенные устройства
    deviceAddedCallback(refCon, data->addedIter);

    // Создаем новый matching dictionary для отключения
    matchingDict = IOServiceMatching(kIOUSBDeviceClassName);
    vendorID = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &(int){0x05AC});
    CFDictionarySetValue(matchingDict, CFSTR(kUSBVendorID), vendorID);
    CFRelease(vendorID);

    // Регистрируем callback для отключения устройств
    kr = IOServiceAddMatchingNotification(
        data->notificationPort,
        kIOTerminatedNotification,
        matchingDict,
        deviceRemovedCallback,
        refCon,
        &data->removedIter
    );

    if (kr != KERN_SUCCESS) {
        return -4;
    }

    // Обрабатываем уже отключенные устройства
    deviceRemovedCallback(refCon, data->removedIter);

    return 0;
}

void runUSBEventLoop(NotificationData *data) {
    CFRunLoopRun();
}

void stopUSBEventLoop(NotificationData *data) {
    if (data->runLoop) {
        CFRunLoopStop(data->runLoop);
    }
}

void cleanupUSBNotifications(NotificationData *data) {
    if (data->addedIter) {
        IOObjectRelease(data->addedIter);
    }
    if (data->removedIter) {
        IOObjectRelease(data->removedIter);
    }
    if (data->notificationPort) {
        IONotificationPortDestroy(data->notificationPort);
    }
}

char* getUSBDeviceProperty(io_service_t service, const char* property) {
    CFStringRef key = CFStringCreateWithCString(kCFAllocatorDefault, property, kCFStringEncodingUTF8);
    CFTypeRef value = IORegistryEntryCreateCFProperty(service, key, kCFAllocatorDefault, 0);
    CFRelease(key);

    if (!value) {
        return NULL;
    }

    if (CFGetTypeID(value) == CFStringGetTypeID()) {
        CFIndex length = CFStringGetLength((CFStringRef)value);
        CFIndex maxSize = CFStringGetMaximumSizeForEncoding(length, kCFStringEncodingUTF8) + 1;
        char *buffer = malloc(maxSize);
        if (buffer && CFStringGetCString((CFStringRef)value, buffer, maxSize, kCFStringEncodingUTF8)) {
            CFRelease(value);
            return buffer;
        }
        if (buffer) free(buffer);
    }

    CFRelease(value);
    return NULL;
}

int getUSBDeviceIntProperty(io_service_t service, const char* property) {
    CFStringRef key = CFStringCreateWithCString(kCFAllocatorDefault, property, kCFStringEncodingUTF8);
    CFTypeRef value = IORegistryEntryCreateCFProperty(service, key, kCFAllocatorDefault, 0);
    CFRelease(key);

    if (!value) {
        return -1;
    }

    int result = -1;
    if (CFGetTypeID(value) == CFNumberGetTypeID()) {
        CFNumberGetValue((CFNumberRef)value, kCFNumberIntType, &result);
    }

    CFRelease(value);
    return result;
}
*/
import "C"

type USBEvent struct {
	Type   string         `json:"type"` // "connected", "disconnected", "state_changed"
	Device *device.Device `json:"device"`
}

type Monitor struct {
	eventChan        chan USBEvent
	stopChan         chan struct{}
	devices          map[string]*device.Device
	devicesMutex     sync.RWMutex
	running          bool
	notificationData *C.NotificationData
	ctx              context.Context
	cancel           context.CancelFunc
}

func NewMonitor() *Monitor {
	return &Monitor{
		eventChan: make(chan USBEvent, 100),
		stopChan:  make(chan struct{}),
		devices:   make(map[string]*device.Device),
	}
}

func (m *Monitor) Start(ctx context.Context) error {
	if m.running {
		return fmt.Errorf("monitor already running")
	}

	m.running = true
	m.ctx, m.cancel = context.WithCancel(ctx)

	log.Println("Starting IOKit USB monitor...")

	// Получаем начальное состояние через cfgutil
	if err := m.initialScan(); err != nil {
		log.Printf("Warning: Initial scan failed: %v", err)
	}

	// Настраиваем IOKit уведомления
	m.notificationData = (*C.NotificationData)(C.malloc(C.sizeof_NotificationData))
	if m.notificationData == nil {
		return fmt.Errorf("failed to allocate notification data")
	}

	result := C.setupUSBNotifications(m.notificationData, unsafe.Pointer(m))
	if result != 0 {
		C.free(unsafe.Pointer(m.notificationData))
		return fmt.Errorf("failed to setup USB notifications: %d", result)
	}

	// Запускаем IOKit event loop в отдельной горутине
	go m.runEventLoop()

	// Запускаем мониторинг состояний устройств
	go m.monitorDeviceStates()

	return nil
}

func (m *Monitor) Stop() {
	if !m.running {
		return
	}

	log.Println("Stopping IOKit USB monitor...")
	m.running = false

	if m.cancel != nil {
		m.cancel()
	}

	close(m.stopChan)

	// Останавливаем IOKit event loop
	if m.notificationData != nil {
		C.stopUSBEventLoop(m.notificationData)
		C.cleanupUSBNotifications(m.notificationData)
		C.free(unsafe.Pointer(m.notificationData))
		m.notificationData = nil
	}
}

func (m *Monitor) Events() <-chan USBEvent {
	return m.eventChan
}

func (m *Monitor) runEventLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("IOKit event loop panic: %v", r)
		}
	}()

	if m.notificationData != nil {
		C.runUSBEventLoop(m.notificationData)
	}
}

func (m *Monitor) monitorDeviceStates() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.checkDeviceStates()
		}
	}
}

func (m *Monitor) checkDeviceStates() {
	cfgutilDevices, err := m.getCfgutilDevices()
	if err != nil {
		return
	}

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	// Проверяем изменения состояния
	for _, currentDev := range cfgutilDevices {
		if existingDev, exists := m.devices[currentDev.SerialNumber]; exists {
			if existingDev.State != currentDev.State || existingDev.IsDFU != currentDev.IsDFU {
				m.devices[currentDev.SerialNumber] = currentDev
				select {
				case m.eventChan <- USBEvent{Type: "state_changed", Device: currentDev}:
				default:
					log.Println("Event channel full, dropping state change event")
				}
			}
		}
	}
}

//export deviceAddedCallback
func deviceAddedCallback(refCon unsafe.Pointer, iterator C.io_iterator_t) {
	monitor := (*Monitor)(refCon)
	if monitor == nil || !monitor.running {
		return
	}

	var service C.io_service_t
	for {
		service = C.IOIteratorNext(iterator)
		if service == 0 {
			break
		}

		device := monitor.parseIOKitDevice(service)
		C.IOObjectRelease(service)

		if device != nil {
			monitor.devicesMutex.Lock()
			if _, exists := monitor.devices[device.SerialNumber]; !exists {
				monitor.devices[device.SerialNumber] = device
				monitor.devicesMutex.Unlock()

				select {
				case monitor.eventChan <- USBEvent{Type: "connected", Device: device}:
				default:
					log.Println("Event channel full, dropping connect event")
				}
			} else {
				monitor.devicesMutex.Unlock()
			}
		}
	}
}

//export deviceRemovedCallback
func deviceRemovedCallback(refCon unsafe.Pointer, iterator C.io_iterator_t) {
	monitor := (*Monitor)(refCon)
	if monitor == nil || !monitor.running {
		return
	}

	var service C.io_service_t
	for {
		service = C.IOIteratorNext(iterator)
		if service == 0 {
			break
		}

		device := monitor.parseIOKitDevice(service)
		C.IOObjectRelease(service)

		if device != nil {
			monitor.devicesMutex.Lock()
			if existingDevice, exists := monitor.devices[device.SerialNumber]; exists {
				delete(monitor.devices, device.SerialNumber)
				monitor.devicesMutex.Unlock()

				select {
				case monitor.eventChan <- USBEvent{Type: "disconnected", Device: existingDevice}:
				default:
					log.Println("Event channel full, dropping disconnect event")
				}
			} else {
				monitor.devicesMutex.Unlock()
			}
		}
	}
}

func (m *Monitor) parseIOKitDevice(service C.io_service_t) *device.Device {
	// Получаем Product ID
	productID := C.getUSBDeviceIntProperty(service, C.CString("idProduct"))
	if productID == -1 {
		return nil
	}

	// Получаем серийный номер
	serialPtr := C.getUSBDeviceProperty(service, C.CString("USB Serial Number"))
	if serialPtr == nil {
		return nil
	}
	serial := C.GoString(serialPtr)
	C.free(unsafe.Pointer(serialPtr))

	if serial == "" {
		return nil
	}

	// Получаем название продукта
	productPtr := C.getUSBDeviceProperty(service, C.CString("USB Product Name"))
	var product string
	if productPtr != nil {
		product = C.GoString(productPtr)
		C.free(unsafe.Pointer(productPtr))
	}

	// Проверяем, что это Mac устройство
	if !m.isMacDevice(product, int(productID)) {
		return nil
	}

	dev := &device.Device{
		SerialNumber: serial,
		Model:        product,
		State:        "connected",
		IsDFU:        m.isDFUDevice(product, int(productID)),
	}

	return dev
}

func (m *Monitor) isMacDevice(productName string, productID int) bool {
	productName = strings.ToLower(productName)

	// Проверяем по названию продукта
	macKeywords := []string{
		"macbook", "imac", "mac mini", "mac studio", "mac pro",
		"dfu", "recovery", "apple t2", "apple t1",
	}

	for _, keyword := range macKeywords {
		if strings.Contains(productName, keyword) {
			return true
		}
	}

	// Известные Product ID для Mac устройств (примеры)
	macProductIDs := []int{
		0x1460, 0x1461, 0x1462, 0x1463, 0x1464, 0x1465, // Mac устройства
		0x1227, 0x1281, // DFU/Recovery режимы
	}

	for _, id := range macProductIDs {
		if productID == id {
			return true
		}
	}

	return false
}

func (m *Monitor) isDFUDevice(productName string, productID int) bool {
	productName = strings.ToLower(productName)

	if strings.Contains(productName, "dfu") || strings.Contains(productName, "recovery") {
		return true
	}

	// DFU/Recovery Product IDs
	dfuProductIDs := []int{
		0x1227, // DFU Mode
		0x1281, // Recovery Mode
	}

	for _, id := range dfuProductIDs {
		if productID == id {
			return true
		}
	}

	return false
}

func (m *Monitor) initialScan() error {
	devices, err := m.getCfgutilDevices()
	if err != nil {
		return err
	}

	m.devicesMutex.Lock()
	defer m.devicesMutex.Unlock()

	for _, dev := range devices {
		m.devices[dev.SerialNumber] = dev
	}

	log.Printf("Initial scan found %d devices", len(devices))
	return nil
}

func (m *Monitor) getCfgutilDevices() ([]*device.Device, error) {
	cmd := exec.Command("cfgutil", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return m.parseCfgutilOutput(string(output)), nil
}

func (m *Monitor) parseCfgutilOutput(output string) []*device.Device {
	var devices []*device.Device
	lines := strings.Split(output, "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "ECID") || strings.HasPrefix(line, "Name") {
			continue
		}

		device := m.parseDeviceLine(line)
		if device != nil {
			devices = append(devices, device)
		}
	}

	return devices
}

func (m *Monitor) parseDeviceLine(line string) *device.Device {
	device := &device.Device{
		IsDFU: false,
	}

	// Парсим формат с табуляцией
	if strings.Contains(line, "\t") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 3 {
			device.SerialNumber = strings.TrimSpace(parts[0])
			device.Model = strings.TrimSpace(parts[1])
			device.State = strings.TrimSpace(parts[2])

			state := strings.ToLower(device.State)
			device.IsDFU = strings.Contains(state, "dfu") || strings.Contains(state, "recovery")

			return device
		}
	}

	// Парсим обычный формат
	parts := strings.Fields(line)
	if len(parts) >= 1 {
		device.SerialNumber = parts[0]

		// Извлекаем модель из скобок
		if start := strings.Index(line, "("); start != -1 {
			if end := strings.Index(line[start:], ")"); end != -1 {
				device.Model = line[start+1 : start+end]
			}
		}

		// Извлекаем состояние после последнего дефиса
		if dashIndex := strings.LastIndex(line, " - "); dashIndex != -1 {
			device.State = strings.TrimSpace(line[dashIndex+3:])
		} else {
			device.State = "Unknown"
		}

		state := strings.ToLower(device.State)
		device.IsDFU = strings.Contains(state, "dfu") || strings.Contains(state, "recovery")

		return device
	}

	return nil
}

func (m *Monitor) GetConnectedDevices() []*device.Device {
	m.devicesMutex.RLock()
	defer m.devicesMutex.RUnlock()

	var devices []*device.Device
	for _, dev := range m.devices {
		devices = append(devices, dev)
	}

	return devices
}
