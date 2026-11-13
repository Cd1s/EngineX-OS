//go:build windows && amd64

package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var winusbDLL = windows.NewLazySystemDLL("winusb.dll")
var cfgmgrDLL = windows.NewLazySystemDLL("cfgmgr32.dll")

type winCalls struct {
	initialize       func(windows.Handle) (uintptr, error)
	free             func(uintptr) error
	query            func(uintptr, uint8) (Interface, error)
	alternate        func(uintptr, uint8) error
	currentAlternate func(uintptr) (uint8, error)
	descriptor       func(uintptr) (uint16, uint16, error)
	policy           func(uintptr, uint8, uint32) error
	transfer         func(uintptr, uint8, bool, *byte, uint32, uint64, *uint32, *windows.Overlapped) error
	result           func(uintptr, *windows.Overlapped, *uint32) error
	cancel           func(windows.Handle, *windows.Overlapped) error
	reset            func(uintptr, uint8) error
	close            func(windows.Handle) error
}

// winProc forwards live Go addresses through uintptr arguments. The directive
// keeps their allocations alive and off a movable caller stack until return.
//
//go:uintptrescapes
func winProc(proc *windows.LazyProc, args ...uintptr) error {
	ok, _, err := proc.Call(args...)
	if ok != 0 {
		return nil
	}
	if err == nil || errors.Is(err, windows.ERROR_SUCCESS) {
		return windows.ERROR_GEN_FAILURE
	}
	return err
}

func nativeWinCalls() (winCalls, error) {
	if err := winusbDLL.Load(); err != nil {
		return winCalls{}, fault("load WinUSB", Binding, false, err)
	}
	names := []string{"WinUsb_Initialize", "WinUsb_Free", "WinUsb_QueryInterfaceSettings", "WinUsb_QueryPipe", "WinUsb_SetCurrentAlternateSetting", "WinUsb_GetCurrentAlternateSetting", "WinUsb_GetDescriptor", "WinUsb_SetPipePolicy", "WinUsb_ReadPipe", "WinUsb_WritePipe", "WinUsb_ControlTransfer", "WinUsb_GetOverlappedResult", "WinUsb_ResetPipe"}
	p := map[string]*windows.LazyProc{}
	for _, name := range names {
		proc := winusbDLL.NewProc(name)
		if err := proc.Find(); err != nil {
			return winCalls{}, fault("resolve WinUSB", Binding, false, err)
		}
		p[name] = proc
	}
	return winCalls{
		initialize: func(file windows.Handle) (uintptr, error) {
			var handle uintptr
			err := winProc(p["WinUsb_Initialize"], uintptr(file), uintptr(unsafe.Pointer(&handle)))
			return handle, err
		},
		free: func(handle uintptr) error { return winProc(p["WinUsb_Free"], handle) },
		alternate: func(handle uintptr, alt uint8) error {
			return winProc(p["WinUsb_SetCurrentAlternateSetting"], handle, uintptr(alt))
		},
		currentAlternate: func(handle uintptr) (uint8, error) {
			var value uint8
			err := winProc(p["WinUsb_GetCurrentAlternateSetting"], handle, uintptr(unsafe.Pointer(&value)))
			return value, err
		},
		query: func(handle uintptr, alt uint8) (Interface, error) {
			var descriptor [9]byte
			if err := winProc(p["WinUsb_QueryInterfaceSettings"], handle, uintptr(alt), uintptr(unsafe.Pointer(&descriptor[0]))); err != nil {
				return Interface{}, err
			}
			if descriptor[0] != 9 || descriptor[1] != 4 {
				return Interface{}, errors.New("invalid native interface descriptor")
			}
			it := Interface{Number: descriptor[2], Alternate: descriptor[3], Class: descriptor[5], Subclass: descriptor[6], Protocol: descriptor[7]}
			for index := 0; index < int(descriptor[4]); index++ {
				var pipe struct {
					Type          uint32
					ID            uint8
					MaxPacketSize uint16
					Interval      uint8
				}
				if err := winProc(p["WinUsb_QueryPipe"], handle, uintptr(alt), uintptr(index), uintptr(unsafe.Pointer(&pipe))); err != nil {
					return Interface{}, err
				}
				if pipe.ID&0x0f == 0 || pipe.ID&0x70 != 0 || pipe.Type > 3 {
					return Interface{}, errors.New("invalid native pipe descriptor")
				}
				for _, ep := range it.Endpoints {
					if ep.Address == pipe.ID {
						return Interface{}, errors.New("duplicate native endpoint")
					}
				}
				it.Endpoints = append(it.Endpoints, Endpoint{Address: pipe.ID, Attributes: uint8(pipe.Type), MaxPacketSize: pipe.MaxPacketSize})
			}
			return it, nil
		},
		descriptor: func(handle uintptr) (uint16, uint16, error) {
			var descriptor [18]byte
			var transferred uint32
			err := winProc(p["WinUsb_GetDescriptor"], handle, 1, 0, 0, uintptr(unsafe.Pointer(&descriptor[0])), 18, uintptr(unsafe.Pointer(&transferred)))
			if err != nil {
				return 0, 0, err
			}
			if transferred != 18 || descriptor[0] != 18 || descriptor[1] != 1 {
				return 0, 0, errors.New("invalid native device descriptor")
			}
			return binary.LittleEndian.Uint16(descriptor[8:10]), binary.LittleEndian.Uint16(descriptor[10:12]), nil
		},
		policy: func(handle uintptr, ep uint8, ms uint32) error {
			// Explicit writes own their packet boundaries; do not append an
			// implicit ZLP to every packet-aligned payload.
			if ep != 0 && ep&0x80 == 0 {
				disabled := uint8(0)
				if err := winProc(p["WinUsb_SetPipePolicy"], handle, uintptr(ep), 1, 1, uintptr(unsafe.Pointer(&disabled))); err != nil {
					return err
				}
			}
			return winProc(p["WinUsb_SetPipePolicy"], handle, uintptr(ep), 3, 4, uintptr(unsafe.Pointer(&ms)))
		},
		transfer: func(handle uintptr, ep uint8, control bool, data *byte, size uint32, setup uint64, done *uint32, overlap *windows.Overlapped) error {
			if control {
				return winProc(p["WinUsb_ControlTransfer"], handle, uintptr(setup), uintptr(unsafe.Pointer(data)), uintptr(size), uintptr(unsafe.Pointer(done)), uintptr(unsafe.Pointer(overlap)))
			}
			proc := p["WinUsb_WritePipe"]
			if ep&0x80 != 0 {
				proc = p["WinUsb_ReadPipe"]
			}
			return winProc(proc, handle, uintptr(ep), uintptr(unsafe.Pointer(data)), uintptr(size), uintptr(unsafe.Pointer(done)), uintptr(unsafe.Pointer(overlap)))
		},
		result: func(handle uintptr, overlap *windows.Overlapped, done *uint32) error {
			return winProc(p["WinUsb_GetOverlappedResult"], handle, uintptr(unsafe.Pointer(overlap)), uintptr(unsafe.Pointer(done)), 1)
		},
		cancel: windows.CancelIoEx,
		reset:  func(handle uintptr, ep uint8) error { return winProc(p["WinUsb_ResetPipe"], handle, uintptr(ep)) },
		close:  windows.CloseHandle,
	}, nil
}

func winError(op string, err error, written bool) error {
	kind := System
	switch {
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		kind = Permission
	case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_BUSY):
		kind = Busy
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_DEVICE_NOT_CONNECTED), errors.Is(err, windows.ERROR_INVALID_HANDLE), errors.Is(err, windows.ERROR_GEN_FAILURE):
		kind = Disconnected
	case errors.Is(err, windows.ERROR_SEM_TIMEOUT), errors.Is(err, windows.ERROR_TIMEOUT):
		kind = Timeout
	case errors.Is(err, windows.ERROR_OPERATION_ABORTED):
		kind = Cancelled
	case errors.Is(err, windows.ERROR_BAD_COMMAND), errors.Is(err, windows.ERROR_INVALID_FUNCTION), errors.Is(err, windows.ERROR_NOT_SUPPORTED):
		kind = Binding
	}
	return fault(op, kind, written, err)
}

type winAttachment struct {
	info     DeviceInfo
	instance string
	devinst  uint32
	arrival  string
	paths    []string
}

type windowsBackend struct {
	mu          sync.Mutex
	attachments map[DeviceID]winAttachment
	calls       winCalls
	scan        func(context.Context) ([]winAttachment, error)
}

func NewBackend() (Backend, error) {
	calls, err := nativeWinCalls()
	if err != nil {
		return nil, err
	}
	return &windowsBackend{calls: calls, scan: scanWindows}, nil
}

func usbHardwareID(ids []string) (uint16, uint16, bool) {
	for _, id := range ids {
		id = strings.ToUpper(id)
		if !strings.HasPrefix(id, "USB\\VID_") {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(id, "USB\\VID_"), "&PID_")
		if len(parts) != 2 || len(parts[0]) != 4 || len(parts[1]) < 4 {
			continue
		}
		vid, err := strconv.ParseUint(parts[0], 16, 16)
		if err != nil {
			continue
		}
		pid, err := strconv.ParseUint(parts[1][:4], 16, 16)
		if err == nil {
			return uint16(vid), uint16(pid), true
		}
	}
	return 0, 0, false
}

func winInterfaces(guids []string, instance string) ([]string, error) {
	instanceUTF, err := windows.UTF16PtrFromString(instance)
	if err != nil {
		return nil, err
	}
	sizeProc := cfgmgrDLL.NewProc("CM_Get_Device_Interface_List_SizeW")
	listProc := cfgmgrDLL.NewProc("CM_Get_Device_Interface_ListW")
	seen := map[string]bool{}
	var result []string
	for _, text := range guids {
		guid, err := windows.GUIDFromString(text)
		if err != nil {
			continue
		}
		for attempt := 0; attempt < 3; attempt++ {
			var size uint32
			status, _, _ := sizeProc.Call(uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(instanceUTF)), 0)
			if status == 0x0d {
				break
			}
			if status != 0 {
				return nil, fmt.Errorf("interface list size CONFIGRET 0x%x", status)
			}
			if size < 2 {
				break
			}
			if size > 65536 {
				return nil, errors.New("interface list exceeds limit")
			}
			buffer := make([]uint16, size)
			status, _, _ = listProc.Call(uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(instanceUTF)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(size), 0)
			if status == 0x1a {
				if attempt == 2 {
					return nil, errors.New("interface list kept changing")
				}
				continue
			}
			if status != 0 {
				return nil, fmt.Errorf("interface list CONFIGRET 0x%x", status)
			}
			for start := 0; start < len(buffer) && buffer[start] != 0; {
				end := start
				for end < len(buffer) && buffer[end] != 0 {
					end++
				}
				if end == len(buffer) {
					return nil, errors.New("unterminated interface path")
				}
				path := windows.UTF16ToString(buffer[start:end])
				key := strings.ToLower(path)
				if !seen[key] {
					seen[key] = true
					result = append(result, path)
				}
				start = end + 1
			}
			break
		}
	}
	return result, nil
}

func registeredGUIDs(set windows.DevInfo, device *windows.DevInfoData) []string {
	result := []string{"{4866319A-F4D6-4374-93B9-DC2DEB361BA9}", "{F72FE0D4-CBCB-407D-8814-9ED673D0DD6B}", "{A5DCBF10-6530-11D2-901F-00C04FB951ED}"}
	handle, err := windows.SetupDiOpenDevRegKey(set, device, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, windows.KEY_READ)
	if err != nil {
		return result
	}
	key := registry.Key(handle)
	defer key.Close()
	if ids, _, err := key.GetStringsValue("DeviceInterfaceGUIDs"); err == nil {
		result = append(result, ids...)
	}
	parameters, err := registry.OpenKey(key, "Device Parameters", registry.QUERY_VALUE)
	if err != nil {
		return result
	}
	defer parameters.Close()
	if ids, _, err := parameters.GetStringsValue("DeviceInterfaceGUIDs"); err == nil {
		result = append(result, ids...)
	}
	return result
}

func winArrival(set windows.DevInfo, device *windows.DevInfoData) string {
	guid, err := windows.GUIDFromString("{83DA6326-97A6-4088-9453-A1923F573B29}")
	if err != nil {
		return ""
	}
	key := windows.DEVPROPKEY{FmtID: windows.DEVPROPGUID(guid), PID: 102}
	var value [8]byte
	var typ, size uint32
	proc := windows.NewLazySystemDLL("setupapi.dll").NewProc("SetupDiGetDevicePropertyW")
	// x/sys' convenience wrapper currently decodes strings only. FILETIME
	// must be read through the raw API to bind an ID to one attachment.
	if err := winProc(proc, uintptr(set), uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&value[0])), 8, uintptr(unsafe.Pointer(&size)), 0); err != nil {
		return ""
	}
	if typ != uint32(windows.DEVPROP_TYPE_FILETIME) || size != 8 {
		return ""
	}
	return fmt.Sprintf("%x", value)
}

func scanWindows(ctx context.Context) ([]winAttachment, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return nil, winError("enumerate SetupAPI", err, false)
	}
	defer set.Close()
	property := func(device *windows.DevInfoData, key windows.SPDRP) any {
		value, _ := set.DeviceRegistryProperty(device, key)
		return value
	}
	var result []winAttachment
	for index := 0; ; index++ {
		if err := ctx.Err(); err != nil {
			return nil, fault("enumerate", Cancelled, false, err)
		}
		device, err := set.EnumDeviceInfo(index)
		if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
			break
		}
		if err != nil {
			return nil, winError("enumerate SetupAPI", err, false)
		}
		ids, _ := property(device, windows.SPDRP_HARDWAREID).([]string)
		vid, pid, ok := usbHardwareID(ids)
		if !ok {
			continue
		}
		instance, err := set.DeviceInstanceID(device)
		if err != nil {
			return nil, winError("device instance", err, false)
		}
		service, _ := property(device, windows.SPDRP_SERVICE).(string)
		product, _ := property(device, windows.SPDRP_DEVICEDESC).(string)
		capabilities, _ := property(device, windows.SPDRP_CAPABILITIES).(uint32)
		serial := ""
		parts := strings.Split(instance, "\\")
		// CM_DEVCAP_UNIQUEID distinguishes USB serials from generated IDs.
		if capabilities&0x10 != 0 && len(parts) == 3 && !strings.Contains(strings.ToUpper(parts[1]), "&MI_") {
			serial = parts[2]
		}
		location, _ := property(device, windows.SPDRP_LOCATION_INFORMATION).(string)
		if locations, ok := property(device, windows.SPDRP_LOCATION_PATHS).([]string); ok && len(locations) > 0 {
			location = locations[0]
		}
		paths, err := winInterfaces(registeredGUIDs(set, device), instance)
		if err != nil {
			return nil, fault("enumerate interface paths", System, false, err)
		}
		arrival := winArrival(set, device)
		result = append(result, winAttachment{info: DeviceInfo{VID: vid, PID: pid, Product: product, Driver: service, Location: location, Serial: serial}, instance: instance, devinst: uint32(device.DevInst), arrival: arrival, paths: paths})
	}
	parentProc := cfgmgrDLL.NewProc("CM_Get_Parent")
	physical := make(map[uint32]DeviceInfo, len(result))
	for _, device := range result {
		physical[device.devinst] = device.info
	}
	for i := range result {
		device := &result[i]
		if device.info.Serial != "" {
			continue
		}
		var parent uint32
		status, _, _ := parentProc.Call(uintptr(unsafe.Pointer(&parent)), uintptr(device.devinst), 0)
		if candidate, ok := physical[parent]; status == 0 && ok && candidate.VID == device.info.VID && candidate.PID == device.info.PID {
			device.info.Serial = candidate.Serial
		}
	}
	return result, nil
}

func (b *windowsBackend) Enumerate(ctx context.Context) ([]DeviceInfo, error) {
	attachments, err := b.scan(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attachments = map[DeviceID]winAttachment{}
	var result []DeviceInfo
	for _, a := range attachments {
		a.info.ID = nextID()
		b.attachments[a.info.ID] = a
		result = append(result, cloneInfo(a.info))
	}
	return result, nil
}

func (b *windowsBackend) revalidate(ctx context.Context, a winAttachment) error {
	if a.arrival == "" {
		return fault("attachment identity", Unsupported, false, errors.New("Windows did not provide an attachment arrival timestamp"))
	}
	current, err := b.scan(ctx)
	if err != nil {
		return err
	}
	for _, c := range current {
		if c.instance == a.instance && c.devinst == a.devinst && c.arrival == a.arrival {
			return nil
		}
	}
	return fault("open", Disconnected, false, errors.New("attachment changed after enumeration"))
}

func (b *windowsBackend) Open(ctx context.Context, id DeviceID, number, alt uint8) (Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, fault("open", Cancelled, false, err)
	}
	b.mu.Lock()
	a, ok := b.attachments[id]
	b.mu.Unlock()
	if !ok {
		return nil, fault("open", Disconnected, false, errors.New("unknown or stale enumeration identity"))
	}
	if !strings.EqualFold(a.info.Driver, "WinUSB") {
		driver := a.info.Driver
		if driver == "" {
			driver = "none"
		}
		return nil, fault("open", Binding, false, fmt.Errorf("current driver is %q; bind the selected update interface to the in-box WinUSB driver", driver))
	}
	if len(a.paths) == 0 {
		return nil, fault("open", Binding, false, errors.New("no registered WinUSB interface path"))
	}
	if err := b.revalidate(ctx, a); err != nil {
		return nil, err
	}
	var failures []error
	for _, path := range a.paths {
		if err := ctx.Err(); err != nil {
			return nil, fault("open", Cancelled, false, err)
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, fault("interface path", Protocol, false, err)
		}
		file, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
		if err != nil {
			failures = append(failures, winError("open interface path", err, false))
			continue
		}
		handle, err := b.calls.initialize(file)
		if err != nil {
			failures = append(failures, errors.Join(winError("WinUsb_Initialize", err, false), winError("close interface path", b.calls.close(file), false)))
			continue
		}
		cleanup := func(e error) {
			failures = append(failures, errors.Join(e, winError("free WinUSB", b.calls.free(handle), false), winError("close interface path", b.calls.close(file), false)))
		}
		it, err := b.calls.query(handle, alt)
		if err != nil {
			cleanup(winError("query interface", err, false))
			continue
		}
		if it.Number != number || it.Alternate != alt || it.Class != 0xff {
			cleanup(fault("select interface", Unsupported, false, errors.New("native interface does not match the selected vendor interface")))
			continue
		}
		vid, pid, err := b.calls.descriptor(handle)
		if err != nil {
			cleanup(winError("read device descriptor", err, false))
			continue
		}
		if vid != a.info.VID || pid != a.info.PID {
			cleanup(fault("open", Disconnected, false, errors.New("native USB VID/PID mismatch")))
			continue
		}
		if err := b.revalidate(ctx, a); err != nil {
			cleanup(err)
			return nil, errors.Join(failures...)
		}
		current, err := b.calls.currentAlternate(handle)
		if err != nil {
			cleanup(winError("get current alternate", err, false))
			continue
		}
		if current != alt {
			if err := b.calls.alternate(handle, alt); err != nil {
				cleanup(winError("set alternate", err, false))
				continue
			}
		}
		a.info.Interfaces = []Interface{it}
		return &windowsDevice{s: newSession(), file: file, handle: handle, calls: b.calls, info: a.info, it: it}, nil
	}
	return nil, errors.Join(failures...)
}

type windowsDevice struct {
	s      *session
	file   windows.Handle
	handle uintptr
	calls  winCalls
	info   DeviceInfo
	it     Interface
}

func (d *windowsDevice) Info() DeviceInfo { return cloneInfo(d.info) }
func (d *windowsDevice) exchange(parent context.Context, ep uint8, data []byte, control bool, setup uint64, write bool) (count int, transferErr error) {
	if err := transferSize(len(data)); err != nil {
		return 0, err
	}
	ctx, end, err := d.s.begin(parent)
	if err != nil {
		return 0, err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return 0, fault("transfer", Cancelled, false, err)
	}
	if err := d.calls.policy(d.handle, ep, timeoutMS(ctx)); err != nil {
		return 0, winError("set transfer timeout", err, false)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, winError("create transfer event", err, false)
	}
	defer windows.CloseHandle(event)
	overlap := &windows.Overlapped{HEvent: event}
	var done uint32
	var dummy byte
	buffer := &dummy
	if len(data) > 0 {
		buffer = &data[0]
	}
	var pin runtime.Pinner
	pin.Pin(buffer)
	pin.Pin(overlap)
	pin.Pin(&done)
	defer pin.Unpin()
	err = d.calls.transfer(d.handle, ep, control, buffer, uint32(len(data)), setup, &done, overlap)
	defer func() {
		if !control && pipeNeedsReset(transferErr) {
			transferErr = errors.Join(transferErr, winError("reset pipe after transfer", d.calls.reset(d.handle, ep), false))
		}
	}()
	if errors.Is(err, windows.ERROR_IO_PENDING) {
		cancelFinished := make(chan struct{})
		var cancelError error
		stop := context.AfterFunc(ctx, func() { cancelError = d.calls.cancel(d.file, overlap); close(cancelFinished) })
		// GetOverlappedResult(wait=true) drains the operation even after cancel.
		err = d.calls.result(d.handle, overlap, &done)
		if !stop() {
			<-cancelFinished
		}
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
			if cancelError != nil && !errors.Is(cancelError, windows.ERROR_NOT_FOUND) {
				err = errors.Join(err, cancelError)
			}
		}
	}
	if done > uint32(len(data)) {
		return 0, fault("transfer", Protocol, write, errors.New("native transfer length exceeds buffer"))
	}
	return int(done), winError("transfer", err, write)
}
func (d *windowsDevice) ReadBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if err := bulkEndpoint(d.it, ep, true); err != nil {
		return 0, err
	}
	return d.exchange(ctx, ep, data, false, 0, false)
}
func (d *windowsDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	if err := bulkEndpoint(d.it, ep, false); err != nil {
		return 0, err
	}
	return d.exchange(ctx, ep, data, false, 0, true)
}
func (d *windowsDevice) Control(ctx context.Context, typ, request uint8, value, index uint16, data []byte) (int, error) {
	if err := controlRequest(d.it, typ, request, index, len(data)); err != nil {
		return 0, err
	}
	setup := uint64(typ) | uint64(request)<<8 | uint64(value)<<16 | uint64(index)<<32 | uint64(len(data))<<48
	return d.exchange(ctx, 0, data, true, setup, typ&0x80 == 0)
}
func (d *windowsDevice) ClearHalt(parent context.Context, ep uint8) error {
	if err := bulkEndpoint(d.it, ep, ep&0x80 != 0); err != nil {
		return err
	}
	ctx, end, err := d.s.begin(parent)
	if err != nil {
		return err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return fault("clear halt", Cancelled, false, err)
	}
	return winError("clear halt", d.calls.reset(d.handle, ep), false)
}
func (d *windowsDevice) Close() error {
	return d.s.close(func() error {
		return errors.Join(winError("free WinUSB", d.calls.free(d.handle), false), winError("close interface path", d.calls.close(d.file), false))
	})
}
