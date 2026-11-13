//go:build darwin && (amd64 || arm64)

package usb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// UUIDBytes has CFUUIDBytes' C layout. QueryInterface passes all sixteen bytes
// by value, which is distinct from passing a pointer on both Darwin ABIs.
type uuidBytes struct{ B0, B1, B2, B3, B4, B5, B6, B7, B8, B9, B10, B11, B12, B13, B14, B15 uint8 }

// Darwin amd64 and arm64 pass this sixteen-byte integer aggregate in two
// integer registers when enough registers remain (as in both calls below).
// Expanding those words avoids purego's byte-field struct argument marshalling.
// This is a by-value argument, never a pointer to temporary UUID storage.
func uuidWords(value uuidBytes) (uint64, uint64) {
	b := [16]byte{value.B0, value.B1, value.B2, value.B3, value.B4, value.B5, value.B6, value.B7, value.B8, value.B9, value.B10, value.B11, value.B12, value.B13, value.B14, value.B15}
	return binary.LittleEndian.Uint64(b[:8]), binary.LittleEndian.Uint64(b[8:])
}

type darwinCOM struct{ Table *[64]uintptr }

type darwinAPI struct {
	matching      func(string) uintptr
	idMatching    func(uint64) uintptr
	services      func(uint32, uintptr, *uint32) int32
	next          func(uint32) uint32
	release       func(uint32) int32
	entryID       func(uint32, *uint64) int32
	parent        func(uint32, string, *uint32) int32
	property      func(uint32, uintptr, uintptr, uint32) uintptr
	cfRelease     func(uintptr)
	cfString      func(uintptr, string, uint32) uintptr
	cfStringRead  func(uintptr, *byte, int64, uint32) bool
	cfNumberRead  func(uintptr, int32, *int64) bool
	cfType        func(uintptr) uintptr
	numberType    func() uintptr
	stringType    func() uintptr
	cfUUID        func(uintptr, uintptr) uintptr
	uuidBytes     func(uintptr) uuidBytes
	uuidFromBytes func(uintptr, uuidBytes) uintptr
	plugin        func(uint32, uintptr, uintptr, **darwinCOM, *int32) int32
}

var darwinOnce sync.Once
var darwinFrameworks *darwinAPI
var darwinLoadError error

func loadDarwin() (*darwinAPI, error) {
	darwinOnce.Do(func() {
		cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			darwinLoadError = fault("load CoreFoundation", Unsupported, false, err)
			return
		}
		io, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			darwinLoadError = errors.Join(fault("load IOKit", Unsupported, false, err), purego.Dlclose(cf))
			return
		}
		a := &darwinAPI{}
		for _, binding := range []struct {
			lib    uintptr
			name   string
			target any
		}{
			{io, "IOServiceMatching", &a.matching}, {io, "IORegistryEntryIDMatching", &a.idMatching}, {io, "IOServiceGetMatchingServices", &a.services},
			{io, "IOIteratorNext", &a.next}, {io, "IOObjectRelease", &a.release}, {io, "IORegistryEntryGetRegistryEntryID", &a.entryID},
			{io, "IORegistryEntryGetParentEntry", &a.parent}, {io, "IORegistryEntryCreateCFProperty", &a.property}, {io, "IOCreatePlugInInterfaceForService", &a.plugin},
			{cf, "CFRelease", &a.cfRelease}, {cf, "CFStringCreateWithCString", &a.cfString}, {cf, "CFStringGetCString", &a.cfStringRead},
			{cf, "CFNumberGetValue", &a.cfNumberRead}, {cf, "CFGetTypeID", &a.cfType}, {cf, "CFNumberGetTypeID", &a.numberType}, {cf, "CFStringGetTypeID", &a.stringType},
			{cf, "CFUUIDCreateFromString", &a.cfUUID}, {cf, "CFUUIDGetUUIDBytes", &a.uuidBytes},
		} {
			symbol, e := purego.Dlsym(binding.lib, binding.name)
			if e != nil {
				darwinLoadError = errors.Join(fault("resolve system USB API", Unsupported, false, e), purego.Dlclose(io), purego.Dlclose(cf))
				return
			}
			purego.RegisterFunc(binding.target, symbol)
		}
		symbol, err := purego.Dlsym(cf, "CFUUIDCreateFromUUIDBytes")
		if err != nil {
			darwinLoadError = errors.Join(err, purego.Dlclose(io), purego.Dlclose(cf))
			return
		}
		var fromWords func(uintptr, uint64, uint64) uintptr
		purego.RegisterFunc(&fromWords, symbol)
		a.uuidFromBytes = func(allocator uintptr, value uuidBytes) uintptr {
			lo, hi := uuidWords(value)
			return fromWords(allocator, lo, hi)
		}
		darwinFrameworks = a
	})
	return darwinFrameworks, darwinLoadError
}

func darwinError(op string, status int32, written bool) error {
	if status == 0 {
		return nil
	}
	kind := System
	switch uint32(status) {
	case 0xe00002c1, 0xe00002e2:
		kind = Permission
	case 0xe00002c5, 0xe00002d5:
		kind = Busy
	case 0xe00002c0, 0xe00002d7, 0xe00002d9, 0xe00002eb, 0xe00002ed:
		kind = Disconnected
	case 0xe00002d6, 0xe0004051:
		kind = Timeout
	case 0xe000404f, 0xe0004061:
		kind = Protocol
	case 0xe00002c7:
		kind = Unsupported
	}
	return fault(op, kind, written, fmt.Errorf("IOKit status 0x%08x", uint32(status)))
}

func (a *darwinAPI) propertyValue(service uint32, name string) uintptr {
	key := a.cfString(0, name, 0x08000100)
	if key == 0 {
		return 0
	}
	defer a.cfRelease(key)
	return a.property(service, key, 0, 0)
}
func (a *darwinAPI) number(service uint32, name string) (uint64, bool) {
	value := a.propertyValue(service, name)
	if value == 0 {
		return 0, false
	}
	defer a.cfRelease(value)
	if a.cfType(value) != a.numberType() {
		return 0, false
	}
	var n int64
	if !a.cfNumberRead(value, 4, &n) || n < 0 {
		return 0, false
	}
	return uint64(n), true
}
func (a *darwinAPI) text(service uint32, name string) string {
	value := a.propertyValue(service, name)
	if value == 0 {
		return ""
	}
	defer a.cfRelease(value)
	if a.cfType(value) != a.stringType() {
		return ""
	}
	buffer := make([]byte, 4096)
	if !a.cfStringRead(value, &buffer[0], int64(len(buffer)), 0x08000100) {
		return ""
	}
	for i, c := range buffer {
		if c == 0 {
			return string(buffer[:i])
		}
	}
	return ""
}
func (a *darwinAPI) uuid(text string) (uintptr, error) {
	s := a.cfString(0, text, 0x08000100)
	if s == 0 {
		return 0, errors.New("cannot create UUID string")
	}
	defer a.cfRelease(s)
	id := a.cfUUID(0, s)
	if id == 0 {
		return 0, errors.New("invalid system interface UUID")
	}
	return id, nil
}
func (a *darwinAPI) walk(ctx context.Context, class string, visit func(uint32) error) error {
	match := a.matching(class)
	if match == 0 {
		return errors.New("cannot create IOKit match")
	}
	var iterator uint32
	if err := darwinError("enumerate", a.services(0, match, &iterator), false); err != nil {
		return err
	}
	defer a.release(iterator)
	for {
		service := a.next(iterator)
		if service == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			a.release(service)
			return fault("enumerate", Cancelled, false, err)
		}
		err := visit(service)
		releaseErr := darwinError("release service", a.release(service), false)
		if err != nil || releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
	}
}
func (a *darwinAPI) service(id uint64) (uint32, error) {
	var iterator uint32
	match := a.idMatching(id)
	if match == 0 {
		return 0, errors.New("cannot create registry ID match")
	}
	if err := darwinError("find attachment", a.services(0, match, &iterator), false); err != nil {
		return 0, err
	}
	defer a.release(iterator)
	service := a.next(iterator)
	if service == 0 {
		return 0, fault("open", Disconnected, false, errors.New("attachment disappeared"))
	}
	return service, nil
}

func (a *darwinAPI) queryInterface(service uint32) (*darwinCOM, error) {
	typeID, err := a.uuid("2D9786C6-9EF3-11D4-AD51-000A27052861")
	if err != nil {
		return nil, err
	}
	defer a.cfRelease(typeID)
	pluginID, err := a.uuid("C244E858-109C-11D4-91D4-0050E4C6426F")
	if err != nil {
		return nil, err
	}
	defer a.cfRelease(pluginID)
	interfaceID, err := a.uuid("4923AC4C-4896-11D5-9208-000A27801E86")
	if err != nil {
		return nil, err
	}
	defer a.cfRelease(interfaceID)
	var plugin *darwinCOM
	var score int32
	if err := darwinError("create USB plugin", a.plugin(service, typeID, pluginID, &plugin, &score), false); err != nil {
		return nil, err
	}
	if plugin == nil {
		return nil, fault("create USB plugin", Binding, false, errors.New("IOKit returned a nil plugin"))
	}
	defer releaseCOM(plugin)
	var query func(*darwinCOM, uint64, uint64, **darwinCOM) int32
	purego.RegisterFunc(&query, plugin.Table[1])
	var iface *darwinCOM
	lo, hi := uuidWords(a.uuidBytes(interfaceID))
	status := query(plugin, lo, hi, &iface)
	if status != 0 {
		return nil, fault("QueryInterface", Binding, false, fmt.Errorf("HRESULT 0x%08x", uint32(status)))
	}
	if iface == nil {
		return nil, fault("QueryInterface", Binding, false, errors.New("IOKit returned a nil USB interface"))
	}
	return iface, nil
}
func releaseCOM(object *darwinCOM) {
	var release func(*darwinCOM) uint32
	purego.RegisterFunc(&release, object.Table[3])
	release(object)
}
func comSimple(object *darwinCOM, index int) int32 {
	var call func(*darwinCOM) int32
	purego.RegisterFunc(&call, object.Table[index])
	return call(object)
}
func comByte(object *darwinCOM, index int) (uint8, error) {
	var call func(*darwinCOM, *uint8) int32
	purego.RegisterFunc(&call, object.Table[index])
	var value uint8
	err := darwinError("query interface", call(object, &value), false)
	return value, err
}
func comWord(object *darwinCOM, index int) (uint16, error) {
	var call func(*darwinCOM, *uint16) int32
	purego.RegisterFunc(&call, object.Table[index])
	var value uint16
	err := darwinError("query device", call(object, &value), false)
	return value, err
}

type darwinAttachment struct {
	info       DeviceInfo
	registryID uint64
	interfaces map[uint8]uint64
}
type darwinBackend struct {
	mu          sync.Mutex
	a           *darwinAPI
	attachments map[DeviceID]darwinAttachment
}

func NewBackend() (Backend, error) {
	a, err := loadDarwin()
	if err != nil {
		return nil, err
	}
	return &darwinBackend{a: a}, nil
}

func (b *darwinBackend) Enumerate(ctx context.Context) ([]DeviceInfo, error) {
	a := b.a
	devices := map[uint64]*darwinAttachment{}
	for _, class := range []string{"IOUSBHostDevice", "IOUSBDevice"} {
		err := a.walk(ctx, class, func(service uint32) error {
			var id uint64
			if e := darwinError("registry identity", a.entryID(service, &id), false); e != nil {
				return e
			}
			if devices[id] != nil {
				return nil
			}
			vid, vok := a.number(service, "idVendor")
			pid, pok := a.number(service, "idProduct")
			if !vok || !pok || vid > 65535 || pid > 65535 {
				return nil
			}
			location := ""
			if value, ok := a.number(service, "locationID"); ok {
				location = fmt.Sprintf("0x%08x", value)
			}
			devices[id] = &darwinAttachment{registryID: id, interfaces: map[uint8]uint64{}, info: DeviceInfo{VID: uint16(vid), PID: uint16(pid), Product: a.text(service, "USB Product Name"), Serial: a.text(service, "USB Serial Number"), Location: location}}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, class := range []string{"IOUSBHostInterface", "IOUSBInterface"} {
		err := a.walk(ctx, class, func(service uint32) error {
			var interfaceID uint64
			if e := darwinError("interface identity", a.entryID(service, &interfaceID), false); e != nil {
				return e
			}
			var parent uint32
			if a.parent(service, "IOService", &parent) != 0 {
				return nil
			}
			defer a.release(parent)
			var parentID uint64
			if a.entryID(parent, &parentID) != 0 {
				return nil
			}
			device := devices[parentID]
			if device == nil {
				return nil
			}
			number, nok := a.number(service, "bInterfaceNumber")
			alt, aok := a.number(service, "bAlternateSetting")
			c, cok := a.number(service, "bInterfaceClass")
			if !nok || !cok || number > 255 || c > 255 {
				return nil
			}
			if !aok {
				alt = 0
			}
			if alt > 255 {
				return nil
			}
			if _, exists := device.interfaces[uint8(number)]; exists {
				return nil
			}
			sub, _ := a.number(service, "bInterfaceSubClass")
			protocol, _ := a.number(service, "bInterfaceProtocol")
			device.interfaces[uint8(number)] = interfaceID
			device.info.Interfaces = append(device.info.Interfaces, Interface{Number: uint8(number), Alternate: uint8(alt), Class: uint8(c), Subclass: uint8(sub), Protocol: uint8(protocol)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attachments = map[DeviceID]darwinAttachment{}
	var result []DeviceInfo
	ids := make([]uint64, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, c := devices[ids[i]], devices[ids[j]]
		if a.info.Location != c.info.Location {
			return a.info.Location < c.info.Location
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		d := devices[id]
		d.info.ID = nextID()
		b.attachments[d.info.ID] = *d
		result = append(result, cloneInfo(d.info))
	}
	return result, nil
}

func (b *darwinBackend) Open(ctx context.Context, id DeviceID, number, alternate uint8) (result Device, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, fault("open", Cancelled, false, err)
	}
	b.mu.Lock()
	attachment, ok := b.attachments[id]
	b.mu.Unlock()
	if !ok {
		return nil, fault("open", Disconnected, false, errors.New("unknown or stale enumeration identity"))
	}
	_, err := selected(attachment.info, number, alternate)
	if err != nil {
		return nil, err
	}
	service, err := b.a.service(attachment.interfaces[number])
	if err != nil {
		return nil, err
	}
	defer b.a.release(service)
	var parent uint32
	if e := darwinError("interface parent", b.a.parent(service, "IOService", &parent), false); e != nil {
		return nil, e
	}
	defer b.a.release(parent)
	var parentID uint64
	if e := darwinError("attachment identity", b.a.entryID(parent, &parentID), false); e != nil {
		return nil, e
	}
	if parentID != attachment.registryID {
		return nil, fault("open", Disconnected, false, errors.New("interface attachment changed"))
	}
	iface, err := b.a.queryInterface(service)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			releaseCOM(iface)
		}
	}()
	vid, err := comWord(iface, 13)
	if err != nil {
		return nil, err
	}
	pid, err := comWord(iface, 14)
	if err != nil {
		return nil, err
	}
	actualNumber, err := comByte(iface, 17)
	if err != nil {
		return nil, err
	}
	if vid != attachment.info.VID || pid != attachment.info.PID || number != actualNumber {
		return nil, fault("open", Disconnected, false, errors.New("native USB identity mismatch"))
	}
	class, err := comByte(iface, 10)
	if err != nil {
		return nil, err
	}
	if class != 0xff {
		return nil, fault("claim interface", Unsupported, false, errors.New("native interface is not vendor-specific"))
	}
	if err := darwinError("claim interface", comSimple(iface, 8), false); err != nil {
		return nil, err
	}
	opened := true
	defer func() {
		if !keep && opened {
			retErr = errors.Join(retErr, darwinError("close failed interface", comSimple(iface, 9), false))
		}
	}()
	current, err := comByte(iface, 18)
	if err != nil {
		return nil, err
	}
	if current != alternate {
		var setAlternate func(*darwinCOM, uint8) int32
		purego.RegisterFunc(&setAlternate, iface.Table[22])
		if err := darwinError("set alternate", setAlternate(iface, alternate), false); err != nil {
			return nil, err
		}
	}
	it, pipes, err := darwinPipeInfo(iface)
	if err != nil {
		return nil, err
	}
	if it.Number != number || it.Alternate != alternate || it.Class != 0xff {
		return nil, fault("open", Protocol, false, errors.New("selected native interface mismatch"))
	}
	if err := ctx.Err(); err != nil {
		return nil, fault("open", Cancelled, false, err)
	}
	attachment.info.Interfaces = []Interface{it}
	keep = true
	opened = false
	return &darwinDevice{s: newSession(), object: iface, info: attachment.info, it: it, pipes: pipes}, nil
}

func darwinPipeInfo(object *darwinCOM) (Interface, map[uint8]uint8, error) {
	var it Interface
	for _, field := range []struct {
		index int
		value *uint8
	}{{10, &it.Class}, {11, &it.Subclass}, {12, &it.Protocol}, {17, &it.Number}, {18, &it.Alternate}} {
		v, err := comByte(object, field.index)
		if err != nil {
			return it, nil, err
		}
		*field.value = v
	}
	count, err := comByte(object, 19)
	if err != nil {
		return it, nil, err
	}
	var properties func(*darwinCOM, uint8, *uint8, *uint8, *uint8, *uint16, *uint8) int32
	purego.RegisterFunc(&properties, object.Table[26])
	pipes := map[uint8]uint8{}
	for i := 1; i <= int(count); i++ {
		var direction, number, typ, interval uint8
		var size uint16
		if err := darwinError("query pipe", properties(object, uint8(i), &direction, &number, &typ, &size, &interval), false); err != nil {
			return it, nil, err
		}
		if number == 0 || number > 15 || direction > 1 {
			return it, nil, fault("query pipe", Protocol, false, errors.New("invalid native pipe descriptor"))
		}
		address := number | direction<<7
		if _, ok := pipes[address]; ok {
			return it, nil, fault("query pipe", Protocol, false, errors.New("duplicate native pipe"))
		}
		pipes[address] = uint8(i)
		it.Endpoints = append(it.Endpoints, Endpoint{Address: address, Attributes: typ, MaxPacketSize: size})
	}
	return it, pipes, nil
}

type darwinDevice struct {
	s      *session
	object *darwinCOM
	info   DeviceInfo
	it     Interface
	pipes  map[uint8]uint8
}

func (d *darwinDevice) Info() DeviceInfo { return cloneInfo(d.info) }

// requestTO mirrors IOUSBDevRequestTO, including pointer-alignment padding.
type darwinRequest struct {
	Type, Request                          uint8
	Value, Index, Length                   uint16
	Data                                   unsafe.Pointer
	Done, NoDataTimeout, CompletionTimeout uint32
}

// The synchronous native method owns the buffer until it returns. Repeating
// AbortPipe after cancellation covers cancellation just before native enqueue;
// no USB payload is replayed. The watcher is joined before releasing COM/data.
func (d *darwinDevice) call(ctx context.Context, pipe uint8, write bool, native func() (int, int32)) (int, error) {
	finished := make(chan struct{})
	watched := make(chan struct{})
	var abortErr error
	go func() {
		defer close(watched)
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		var abort func(*darwinCOM, uint8) int32
		purego.RegisterFunc(&abort, d.object.Table[28])
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			status := abort(d.object, pipe)
			if status != 0 && abortErr == nil {
				abortErr = darwinError("abort pipe", status, write)
			}
			select {
			case <-finished:
				return
			case <-ticker.C:
			}
		}
	}()
	n, status := native()
	close(finished)
	<-watched
	if err := ctx.Err(); err != nil {
		return n, errors.Join(fault("transfer", Cancelled, write, err), abortErr)
	}
	return n, darwinError("transfer", status, write)
}

func (d *darwinDevice) bulk(parent context.Context, ep uint8, data []byte, in bool) (count int, transferErr error) {
	if err := bulkEndpoint(d.it, ep, in); err != nil {
		return 0, err
	}
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
	var dummy byte
	buffer := unsafe.Pointer(&dummy)
	var pin runtime.Pinner
	if len(data) > 0 {
		buffer = unsafe.Pointer(&data[0])
	}
	pin.Pin(buffer)
	defer pin.Unpin()
	pipe := d.pipes[ep]
	defer func() {
		if pipeNeedsReset(transferErr) {
			transferErr = errors.Join(transferErr, d.resetPipe(pipe, "reset pipe after transfer"))
		}
	}()
	size := uint32(len(data))
	ms := timeoutMS(ctx)
	if in {
		var read func(*darwinCOM, uint8, unsafe.Pointer, *uint32, uint32, uint32) int32
		purego.RegisterFunc(&read, d.object.Table[39])
		n, err := d.call(ctx, pipe, false, func() (int, int32) {
			status := read(d.object, pipe, buffer, &size, ms, ms)
			if status != 0 {
				return 0, status
			}
			return int(size), status
		})
		if n > len(data) {
			return 0, fault("read", Protocol, false, errors.New("native transfer length exceeds buffer"))
		}
		return n, err
	}
	var write func(*darwinCOM, uint8, unsafe.Pointer, uint32, uint32, uint32) int32
	purego.RegisterFunc(&write, d.object.Table[40])
	return d.call(ctx, pipe, true, func() (int, int32) {
		status := write(d.object, pipe, buffer, size, ms, ms)
		if status != 0 {
			return 0, status
		}
		return len(data), status
	})
}
func (d *darwinDevice) ReadBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	return d.bulk(ctx, ep, data, true)
}
func (d *darwinDevice) WriteBulk(ctx context.Context, ep uint8, data []byte) (int, error) {
	return d.bulk(ctx, ep, data, false)
}
func (d *darwinDevice) Control(parent context.Context, typ, request uint8, value, index uint16, data []byte) (int, error) {
	if err := controlRequest(d.it, typ, request, index, len(data)); err != nil {
		return 0, err
	}
	ctx, end, err := d.s.begin(parent)
	if err != nil {
		return 0, err
	}
	defer end()
	if err := ctx.Err(); err != nil {
		return 0, fault("control", Cancelled, false, err)
	}
	ms := timeoutMS(ctx)
	req := &darwinRequest{Type: typ, Request: request, Value: value, Index: index, Length: uint16(len(data)), NoDataTimeout: ms, CompletionTimeout: ms}
	var pin runtime.Pinner
	pin.Pin(req)
	defer pin.Unpin()
	if len(data) > 0 {
		pin.Pin(&data[0])
		req.Data = unsafe.Pointer(&data[0])
	}
	var control func(*darwinCOM, uint8, *darwinRequest) int32
	purego.RegisterFunc(&control, d.object.Table[37])
	n, err := d.call(ctx, 0, typ&0x80 == 0, func() (int, int32) {
		status := control(d.object, 0, req)
		if status != 0 {
			return 0, status
		}
		return int(req.Done), status
	})
	if n > len(data) {
		return 0, fault("control", Protocol, typ&0x80 == 0, errors.New("native control length exceeds buffer"))
	}
	return n, err
}

func (d *darwinDevice) resetPipe(pipe uint8, op string) error {
	var clear func(*darwinCOM, uint8) int32
	purego.RegisterFunc(&clear, d.object.Table[30])
	return darwinError(op, clear(d.object, pipe), false)
}
func (d *darwinDevice) ClearHalt(parent context.Context, ep uint8) error {
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
	return d.resetPipe(d.pipes[ep], "clear halt")
}
func (d *darwinDevice) Close() error {
	return d.s.close(func() error {
		err := darwinError("release interface", comSimple(d.object, 9), false)
		releaseCOM(d.object)
		return err
	})
}
