//go:build windows && amd64

package usb

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ShellExecuteEx runs only the device-setup helper elevated. The caller keeps
// its console and resumes normal installation after that helper exits.
func elevateDriverSetup(instance, arrival string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args := []string{WindowsDriverSetupArgument, instance, arrival}
	for i := range args {
		args[i] = syscall.EscapeArg(args[i])
	}
	parameters, err := windows.UTF16PtrFromString(strings.Join(args, " "))
	if err != nil {
		return err
	}
	var request struct {
		Size, Mask                        uint32
		Window                            windows.Handle
		Verb, File, Parameters, Directory *uint16
		Show                              int32
		Instance                          windows.Handle
		IDList                            unsafe.Pointer
		Class                             *uint16
		ClassKey                          windows.Handle
		HotKey                            uint32
		Icon                              windows.Handle
		Process                           windows.Handle
	}
	request.Size, request.Mask = uint32(unsafe.Sizeof(request)), 0x40 // SEE_MASK_NOCLOSEPROCESS
	request.Verb, request.File, request.Parameters = verb, file, parameters
	proc := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")
	if err := winProc(proc, uintptr(unsafe.Pointer(&request))); err != nil {
		return err
	}
	defer windows.CloseHandle(request.Process)
	if _, err := windows.WaitForSingleObject(request.Process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(request.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		return syscall.Errno(code)
	}
	return nil
}
