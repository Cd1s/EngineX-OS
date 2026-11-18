package cli

import (
	"fmt"
	"io"

	"github.com/Cd1s/EngineX-OS/pkg/wifi"
)

const iwUsage = `Usage: enginex-suite iw dev <interface> get power_save
       enginex-suite iw dev <interface> set power_save <on|off>
`

// IW is the power-save subset used when a deck has no stock iw. It is never
// installed under the global iw name, which belongs to the vendor's full tool.
func IW(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, iwUsage)
			return 0
		case "-v", "--version":
			fmt.Fprintln(stdout, "iw (EngineX OS power-save subset)")
			return 0
		}
	}
	if len(args) == 4 && args[0] == "dev" && args[2] == "get" && args[3] == "power_save" {
		on, err := wifi.GetPowerSave(args[1])
		if err != nil {
			fmt.Fprintf(stderr, "iw: %v\n", err)
			return 1
		}
		state := "off"
		if on {
			state = "on"
		}
		fmt.Fprintln(stdout, "Power save:", state)
		return 0
	}
	if len(args) == 5 && args[0] == "dev" && args[2] == "set" && args[3] == "power_save" && (args[4] == "on" || args[4] == "off") {
		if err := wifi.SetPowerSave(args[1], args[4] == "on"); err != nil {
			fmt.Fprintf(stderr, "iw: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, iwUsage)
	return 2
}
