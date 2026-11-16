//go:build windows && amd64

package usb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const WindowsDriverSetupArgument = "--internal-winusb-setup"
const updateInterfaceGUID = "{4866319A-F4D6-4374-93B9-DC2DEB361BA9}"

func windowsUpdateFamily(vid, pid uint16) string {
	switch vid {
	case 0x15e4:
		switch pid {
		case 0xd006, 0xd00a, 0xd00c, 0xd00d, 0xd00e, 0xd008, 0xd00b, 0x203f,
			0xd007, 0xc00c, 0xd011, 0xd012, 0x103f, 0x303f, 0x2059:
			return "fastboot"
		}
	case 0x18d1:
		if pid == 0x4ee0 {
			return "fastboot"
		}
	case 0x2207:
		switch pid {
		case 0x320a, 0x350b:
			return "rockchip"
		case 0x0006:
			return "fastboot"
		}
	case 0x1cc5:
		if pid == 0x2027 {
			return "azupdate"
		}
	}
	return ""
}

func windowsCompatibleInterface(instance string, ids []string) []Interface {
	var number uint8
	if _, tail, ok := strings.Cut(strings.ToUpper(instance), "&MI_"); ok {
		fmt.Sscanf(tail, "%02X", &number)
	}
	for _, id := range ids {
		var class, subclass, protocol uint8
		if n, _ := fmt.Sscanf(strings.ToUpper(id), `USB\CLASS_%02X&SUBCLASS_%02X&PROT_%02X`, &class, &subclass, &protocol); n == 3 && class != 0 {
			return []Interface{{Number: number, Class: class, Subclass: subclass, Protocol: protocol}}
		}
	}
	return nil
}

func canSetupWindows(a winAttachment) bool {
	family := windowsUpdateFamily(a.info.VID, a.info.PID)
	if family == "" || strings.EqualFold(a.info.Driver, "usbccgp") {
		return false
	}
	for _, it := range a.info.Interfaces {
		if it.Class == 0xff && ((family == "fastboot" && it.Subclass == 0x42 && it.Protocol == 3) ||
			(family == "azupdate" && it.Subclass == 0x61 && it.Protocol == 0x7a) || family == "rockchip") {
			return true
		}
	}
	return len(a.info.Interfaces) == 0 && strings.EqualFold(a.info.Driver, "WinUSB")
}

// selectInboxWinUSB selects Microsoft's existing signed INF. The caller owns
// the driver list until DiInstallDevice returns; no driver package is added.
func selectInboxWinUSB(set windows.DevInfo, device *windows.DevInfoData) (*windows.DrvInfoData, error) {
	dir, err := windows.GetWindowsDirectory()
	if err != nil {
		return nil, err
	}
	// A device's current setup class may exclude WinUSB. Build globally, then
	// inherit that list for this device before selecting the in-box driver.
	params, err := set.DeviceInstallParams(nil)
	if err != nil {
		return nil, err
	}
	params.Flags |= windows.DI_ENUMSINGLEINF
	params.FlagsEx |= windows.DI_FLAGSEX_ALLOWEXCLUDEDDRVS
	if err := params.SetDriverPath(filepath.Join(dir, "INF", "winusb.inf")); err != nil {
		return nil, err
	}
	if err := set.SetDeviceInstallParams(nil, params); err != nil {
		return nil, err
	}
	if err := set.BuildDriverInfoList(nil, windows.SPDIT_CLASSDRIVER); err != nil {
		return nil, err
	}
	if device != nil {
		instance, err := set.DeviceInstanceID(device)
		if err != nil {
			return nil, err
		}
		id, err := windows.UTF16PtrFromString(instance)
		if err != nil {
			return nil, err
		}
		proc := windows.NewLazySystemDLL("setupapi.dll").NewProc("SetupDiOpenDeviceInfoW")
		if err := winProc(proc, uintptr(set), uintptr(unsafe.Pointer(id)), 0, 2, uintptr(unsafe.Pointer(device))); err != nil { // DIOD_INHERIT_CLASSDRVS
			return nil, err
		}
	}
	for index := 0; ; index++ {
		driver, err := set.EnumDriverInfo(device, windows.SPDIT_CLASSDRIVER, index)
		if err != nil {
			return nil, err
		}
		detail, err := set.DriverInfoDetail(device, driver)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(detail.SectionName(), "WINUSB") {
			return driver, nil
		}
	}
}

// ConfigureWindowsDriver is the elevated helper's only operation. The driver
// store and firmware are untouched; only this update attachment is configured.
func ConfigureWindowsDriver(ctx context.Context, instance, arrival string) error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return windows.ERROR_ELEVATION_REQUIRED
	}
	attachments, err := scanWindows(ctx)
	if err != nil {
		return err
	}
	var selected *winAttachment
	for i := range attachments {
		if attachments[i].instance == instance && attachments[i].arrival == arrival && arrival != "" {
			selected = &attachments[i]
			break
		}
	}
	if selected == nil {
		return errors.New("USB attachment changed before driver setup")
	}
	if !canSetupWindows(*selected) {
		return errors.New("the selected device is not a supported USB update interface")
	}
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return err
	}
	defer set.Close()
	for index := 0; ; index++ {
		device, err := set.EnumDeviceInfo(index)
		if err != nil {
			return err
		}
		if uint32(device.DevInst) != selected.devinst || winArrival(set, device) != arrival {
			continue
		}
		var pin runtime.Pinner
		pin.Pin(device)
		handle, _, keyErr := windows.NewLazySystemDLL("setupapi.dll").NewProc("SetupDiCreateDevRegKeyW").Call(uintptr(set), uintptr(unsafe.Pointer(device)), uintptr(windows.DICS_FLAG_GLOBAL), 0, uintptr(windows.DIREG_DEV), 0, 0)
		pin.Unpin()
		if handle == ^uintptr(0) {
			return fmt.Errorf("create device hardware key: %w", keyErr)
		}
		key := registry.Key(handle)
		defer key.Close()
		guids, _, err := key.GetStringsValue("DeviceInterfaceGUIDs")
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return err
		}
		present := false
		for _, guid := range guids {
			present = present || strings.EqualFold(guid, updateInterfaceGUID)
		}
		if !present {
			if err := key.SetStringsValue("DeviceInterfaceGUIDs", append(guids, updateInterfaceGUID)); err != nil {
				return err
			}
		}
		if !strings.EqualFold(selected.info.Driver, "WinUSB") {
			defer set.DestroyDriverInfoList(nil, windows.SPDIT_CLASSDRIVER)
			driver, err := selectInboxWinUSB(set, device)
			if err != nil {
				return err
			}
			if err := set.SetSelectedDriver(device, driver); err != nil {
				return err
			}
			var reboot int32
			proc := windows.NewLazySystemDLL("newdev.dll").NewProc("DiInstallDevice")
			if err := winProc(proc, 0, uintptr(set), uintptr(unsafe.Pointer(device)), uintptr(unsafe.Pointer(driver)), 0, uintptr(unsafe.Pointer(&reboot))); err != nil {
				return err
			}
			if reboot != 0 {
				return windows.ERROR_SUCCESS_REBOOT_REQUIRED
			}
			return nil
		}
		change := windows.PropChangeParams{ClassInstallHeader: *windows.MakeClassInstallHeader(windows.DIF_PROPERTYCHANGE), StateChange: windows.DICS_PROPCHANGE, Scope: windows.DICS_FLAG_CONFIGSPECIFIC}
		if err := set.SetClassInstallParams(device, &change.ClassInstallHeader, uint32(unsafe.Sizeof(change))); err != nil {
			return err
		}
		return set.CallClassInstaller(windows.DIF_PROPERTYCHANGE, device)
	}
}

func prepareWindows(ctx context.Context, a winAttachment) (winAttachment, error) {
	if !canSetupWindows(a) {
		return a, errors.New("only a supported vendor update interface may be configured")
	}
	fmt.Fprintln(os.Stderr, "正在为选中的 USB 更新接口配置 Windows 内置 WinUSB 驱动及接口注册；需要时请批准管理员授权。撤销方法：在设备管理器卸载这个更新设备，再拔插 USB。\nConfiguring Microsoft's in-box WinUSB driver and interface registration for the selected USB update interface. Approve administrator access if requested. Undo: uninstall this update device in Device Manager, then unplug and reconnect USB.")
	var err error
	if windows.GetCurrentProcessToken().IsElevated() {
		err = ConfigureWindowsDriver(ctx, a.instance, a.arrival)
	} else {
		err = elevateDriverSetup(a.instance, a.arrival)
	}
	if err != nil {
		if errors.Is(err, windows.ERROR_SUCCESS_REBOOT_REQUIRED) {
			return a, errors.New("Windows 驱动配置需要重启电脑；重启后再运行安装器，没有刷写固件 / Windows driver setup requires a PC restart; restart and run the installer again; nothing flashed")
		}
		return a, fmt.Errorf("Windows USB 驱动配置未完成，没有刷写固件 / Windows USB driver setup did not finish; nothing flashed: %w", err)
	}
	// PnP publishes the new device interface after the install/restart completes.
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		current, err := scanWindows(ctx)
		if err != nil {
			return a, err
		}
		for _, fresh := range current {
			if fresh.instance == a.instance && strings.EqualFold(fresh.info.Driver, "WinUSB") && len(fresh.paths) != 0 {
				fresh.info.ID = a.info.ID
				return fresh, nil
			}
		}
		select {
		case <-ctx.Done():
			return a, ctx.Err()
		case <-deadline.C:
			return a, errors.New("WinUSB setup completed but the interface is unavailable; disconnect/reconnect USB or restart Windows, then retry; nothing flashed")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
