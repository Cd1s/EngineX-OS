// Package cpuram reads Linux /proc/stat and /proc/meminfo for the CPU & RAM Load Mod.
package cpuram

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type feature struct {
	statPath    string
	meminfoPath string

	mu        sync.Mutex
	hasPrev   bool
	prevTotal uint64
	prevIdle  uint64
}

// New returns the CPU & RAM monitor feature.
func New(_ modkit.Env) (modkit.Feature, error) {
	return &feature{
		statPath:    "/proc/stat",
		meminfoPath: "/proc/meminfo",
	}, nil
}

func (f *feature) Status() (any, error) {
	total, idle, err := readCPUStat(f.statPath)
	if err != nil {
		return nil, err
	}
	memTotalKB, memUsedKB, err := readMemInfo(f.meminfoPath)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	var cpuPct float64
	if f.hasPrev && total > f.prevTotal {
		dTotal := total - f.prevTotal
		var dIdle uint64
		if idle >= f.prevIdle {
			dIdle = idle - f.prevIdle
		}
		if dIdle > dTotal {
			dIdle = dTotal
		}
		cpuPct = round1(float64(dTotal-dIdle) * 100.0 / float64(dTotal))
	}
	f.prevTotal = total
	f.prevIdle = idle
	f.hasPrev = true
	f.mu.Unlock()

	var ramPct float64
	if memTotalKB > 0 {
		ramPct = round1(float64(memUsedKB) * 100.0 / float64(memTotalKB))
	}
	return map[string]any{
		"cpuPercent": cpuPct,
		"ramPercent": ramPct,
		"ramUsedMB":  memUsedKB / 1024,
		"ramTotalMB": memTotalKB / 1024,
	}, nil
}

func (f *feature) Action(_ map[string]any) (any, error) {
	return nil, modkit.Bad("unknown action")
}

func (f *feature) Close() {}

func round1(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return math.Round(v*10) / 10
}

func readCPUStat(path string) (total, idle uint64, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("unexpected /proc/stat header")
	}
	for i, s := range fields[1:] {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += v
		// fields[1:] indices: 0=user, 1=nice, 2=system, 3=idle, 4=iowait
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle, nil
}

func readMemInfo(path string) (totalKB, usedKB uint64, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var memFree, buffers, cached, memAvail uint64
	var hasTotal, hasAvail bool
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB = v
			hasTotal = true
		case "MemAvailable":
			memAvail = v
			hasAvail = true
		case "MemFree":
			memFree = v
		case "Buffers":
			buffers = v
		case "Cached":
			cached = v
		}
	}
	if !hasTotal || totalKB == 0 {
		return 0, 0, fmt.Errorf("missing MemTotal in %s", path)
	}
	if !hasAvail {
		memAvail = memFree + buffers + cached
	}
	if memAvail > totalKB {
		memAvail = totalKB
	}
	return totalKB, totalKB - memAvail, nil
}
