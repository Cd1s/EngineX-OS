//go:build windows && amd64

package usb

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsSetupTargetsOnlyUpdateInterfaces(t *testing.T) {
	for _, c := range []struct {
		name               string
		vid, pid           uint16
		driver, compatible string
		want               bool
	}{
		{"Gen-2 fastboot", 0x15e4, 0x2059, "", `USB\Class_ff&SubClass_42&Prot_03`, true},
		{"AZ04", 0x1cc5, 0x2027, "", `USB\Class_ff&SubClass_61&Prot_7a`, true},
		{"RK3288 loader", 0x2207, 0x320a, "Rockusb", `USB\Class_ff&SubClass_06&Prot_05`, true},
		{"RK3588 loader", 0x2207, 0x350b, "Rockusb", `USB\Class_ff&SubClass_06&Prot_05`, true},
		{"missing GUID only", 0x15e4, 0x2059, "WinUSB", "", true},
		{"composite parent", 0x15e4, 0x2059, "usbccgp", "", false},
		{"audio", 0x15e4, 0x2059, "usbaudio", `USB\Class_01&SubClass_01&Prot_00`, false},
		{"HID", 0x15e4, 0x2059, "HidUsb", `USB\Class_03&SubClass_01&Prot_01`, false},
		{"storage", 0x15e4, 0x2059, "USBSTOR", `USB\Class_08&SubClass_06&Prot_50`, false},
		{"ADB", 0x2207, 0x0006, "WinUSB", `USB\Class_ff&SubClass_42&Prot_01`, false},
		{"other vendor", 0x1234, 0x2059, "WinUSB", `USB\Class_ff&SubClass_42&Prot_03`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			instance := fmt.Sprintf(`USB\VID_%04X&PID_%04X&MI_03\fixture`, c.vid, c.pid)
			a := winAttachment{info: DeviceInfo{VID: c.vid, PID: c.pid, Driver: c.driver, Interfaces: windowsCompatibleInterface(instance, []string{c.compatible})}}
			if got := canSetupWindows(a); got != c.want {
				t.Fatalf("setup allowed=%t, want %t", got, c.want)
			}
			if len(a.info.Interfaces) != 0 && a.info.Interfaces[0].Number != 3 {
				t.Fatal("composite interface number lost")
			}
		})
	}
}

func TestWindowsInboxWinUSBDriverSelection(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("creating an unregistered test element requires administrator rights")
	}
	set, err := windows.SetupDiCreateDeviceInfoListEx(nil, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	defer set.DestroyDriverInfoList(nil, windows.SPDIT_CLASSDRIVER)
	driver, err := selectInboxWinUSB(set, nil)
	if err != nil {
		t.Fatal(err)
	}
	// This element is never registered with PnP or installed. It inherits the
	// global list just as OpenDeviceInfo does for the selected real attachment.
	device, err := set.CreateDeviceInfo("EngineXInstallerTest", &windows.GUID{}, "", 0, windows.DICD_GENERATE_ID|windows.DICD_INHERIT_CLASSDRVS)
	if err != nil {
		t.Fatal(err)
	}
	driver, err = set.EnumDriverInfo(device, windows.SPDIT_CLASSDRIVER, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.SetSelectedDriver(device, driver); err != nil {
		t.Fatal(err)
	}
	detail, err := set.DriverInfoDetail(nil, driver)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("selected signed in-box driver section %s, without installing it", detail.SectionName())
}

func TestWindowsInboxDriverListForPresentUSBNode(t *testing.T) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "USB", 0, windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	device, err := set.EnumDeviceInfo(0)
	if err == windows.ERROR_NO_MORE_ITEMS {
		t.Skip("no USB node is present")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer set.DestroyDriverInfoList(nil, windows.SPDIT_CLASSDRIVER)
	driver, err := selectInboxWinUSB(set, device)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := set.DriverInfoDetail(device, driver)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inherited in-box section %s; no driver selected or installed on the real USB node", detail.SectionName())
}
