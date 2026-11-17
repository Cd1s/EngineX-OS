package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Cd1s/EngineX-OS/pkg/wifi"
)

const iwUsage = `Usage:	iw [options] dev <devname> get power_save
	iw [options] dev <devname> set power_save <on|off>
`

// IW implements the subset of iw(8) needed by EngineX scripts (enginex-debloat)
// and SSH operators to query and toggle Wi-Fi power saving.
func IW(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, iwUsage)
		return 2
	}

	var cleanArgs []string
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, iwUsage)
			return 0
		case "-v", "--version":
			fmt.Fprintln(stdout, "iw (EngineX OS nl80211 subset)")
			return 0
		default:
			if !strings.HasPrefix(a, "-") {
				cleanArgs = append(cleanArgs, a)
			}
		}
	}

	if len(cleanArgs) < 2 {
		fmt.Fprint(stderr, iwUsage)
		return 2
	}

	devName := ""
	rest := cleanArgs
	if rest[0] == "dev" {
		if len(rest) < 2 {
			fmt.Fprint(stderr, iwUsage)
			return 2
		}
		devName = rest[1]
		rest = rest[2:]
	} else {
		devName = rest[0]
		rest = rest[1:]
	}

	if len(rest) == 0 {
		fmt.Fprint(stderr, iwUsage)
		return 2
	}

	action := rest[0]
	switch action {
	case "get":
		if len(rest) < 2 || rest[1] != "power_save" {
			fmt.Fprintf(stderr, "command %q %s not supported\n", action, strings.Join(rest[1:], " "))
			return 2
		}
		enabled, err := wifi.GetPowerSave(devName)
		if err != nil {
			fmt.Fprintf(stderr, "iw: %v\n", err)
			return 1
		}
		if enabled {
			fmt.Fprintln(stdout, "Power save: on")
		} else {
			fmt.Fprintln(stdout, "Power save: off")
		}
		return 0

	case "set":
		if len(rest) < 3 || rest[1] != "power_save" {
			fmt.Fprintf(stderr, "command %q %s not supported\n", action, strings.Join(rest[1:], " "))
			return 2
		}
		var enable bool
		switch rest[2] {
		case "on", "1", "enable", "enabled":
			enable = true
		case "off", "0", "disable", "disabled":
			enable = false
		default:
			fmt.Fprintf(stderr, "unknown power save state %q; use 'on' or 'off'\n", rest[2])
			return 2
		}
		if err := wifi.SetPowerSave(devName, enable); err != nil {
			fmt.Fprintf(stderr, "iw: %v\n", err)
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "command %q not supported\n", action)
		return 2
	}
}
