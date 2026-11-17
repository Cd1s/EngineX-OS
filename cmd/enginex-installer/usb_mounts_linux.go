package main

import (
	"os"
	"strings"
)

func usbMountPaths() ([]string, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	var paths []string
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 6 {
			paths = append(paths, unescape.Replace(fields[4]))
		}
	}
	return paths, nil
}
