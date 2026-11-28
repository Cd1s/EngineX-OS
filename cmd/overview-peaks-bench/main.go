// Command overview-peaks-bench runs ONLY the streamed-track per-bin peak
// overview computation on a single audio file under stream-sync's exact
// resourceguard constraints (CPU3, SCHED_IDLE, nice 19, IOPRIO_CLASS_IDLE),
// printing wall time, sleep time, peak RSS, and bin statistics.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/analyzer"
	"github.com/Cd1s/EngineX-OS/pkg/resourceguard"
)

func main() {
	cfg := resourceguard.DefaultConfig()
	flag.IntVar(&cfg.CPU, "cpu", cfg.CPU, "dedicated background CPU")
	flag.DurationVar(&cfg.ThrottleWork, "throttle-work", cfg.ThrottleWork, "work interval before cooperative sleep")
	flag.DurationVar(&cfg.ThrottleSleep, "throttle-sleep", cfg.ThrottleSleep, "cooperative sleep interval")
	flag.BoolVar(&cfg.Disabled, "disable-guard", false, "disable CPU affinity / idle priority controls")
	flag.BoolVar(&cfg.StrictCPU, "strict-cpu", false, "fail if dedicated CPU is unavailable")
	outPath := flag.String("out", "", "optional path to write EXOV1 sidecar payload")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: overview-peaks-bench [flags] <audio-file>\n")
		os.Exit(2)
	}
	audioPath := flag.Arg(0)

	// Match stream-sync's single-thread CPU budget.
	runtime.GOMAXPROCS(1)
	guard := resourceguard.New(cfg)
	if _, err := guard.ApplyToProcess(0); err != nil {
		fmt.Fprintf(os.Stderr, "apply guard: %v\n", err)
		os.Exit(1)
	}

	t0 := time.Now()
	fp, fpErr := analyzer.OverviewFileFingerprint(audioPath)
	var ov *analyzer.OverviewPeaks
	var slept time.Duration
	err := guard.Run(context.Background(), func(th *resourceguard.Throttle) error {
		var cerr error
		ov, cerr = analyzer.ComputeOverviewPeaks(context.Background(), audioPath, th)
		slept = th.Slept
		return cerr
	})
	elapsed := time.Since(t0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ComputeOverviewPeaks failed after %s: %v\n", elapsed.Round(time.Millisecond), err)
		os.Exit(1)
	}

	if *outPath != "" {
		sidecar, err := analyzer.EncodeOverviewSidecar(ov.Raw[:], ov.Compressed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "EncodeOverviewSidecar: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*outPath, sidecar, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write %s: %v\n", *outPath, err)
			os.Exit(1)
		}
	}

	p50, p90 := binPercentiles(ov.Raw[:])
	fmt.Printf("overview-peaks-bench file=%s\n", audioPath)
	if fpErr == nil {
		fmt.Printf("  fingerprint=%s\n", fp)
	}
	fmt.Printf("  guard=%s\n", guard.LastReport().String())
	fmt.Printf("  format=%s sr=%d ch=%d samples=%d duration=%.2fs\n",
		ov.Format, ov.SampleRate, ov.Channels, ov.Samples, float64(ov.Samples)/float64(ov.SampleRate))
	fmt.Printf("  wall=%s (%.3fs) slept=%s rss_hwm=%s zlen=%d\n",
		elapsed.Round(time.Millisecond), elapsed.Seconds(), slept.Round(time.Millisecond), readVmHWM(), len(ov.Compressed))
	fmt.Printf("  trailer=(%d,%d,%d) p50/max=(%.1f%%,%.1f%%,%.1f%%) p90/max=(%.1f%%,%.1f%%,%.1f%%)\n",
		ov.Trailer[0], ov.Trailer[1], ov.Trailer[2],
		p50[0], p50[1], p50[2], p90[0], p90[1], p90[2])
}

func binPercentiles(raw []byte) (p50, p90 [3]float64) {
	for c := 0; c < 3; c++ {
		maxV := float64(raw[analyzer.OverviewRawSize-3+c])
		if maxV <= 0 {
			continue
		}
		vals := make([]int, analyzer.OverviewEntries)
		for i := 0; i < analyzer.OverviewEntries; i++ {
			vals[i] = int(raw[24+i*3+c])
		}
		sort.Ints(vals)
		p50[c] = float64(vals[analyzer.OverviewEntries/2]) / maxV * 100
		p90[c] = float64(vals[analyzer.OverviewEntries*9/10]) / maxV * 100
	}
	return p50, p90
}

func readVmHWM() string {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
		}
	}
	return "unknown"
}
