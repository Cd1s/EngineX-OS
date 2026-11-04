//go:build linux

package resourceguard

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRunPinsWorkerToCPU3WithIdlePolicy(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skip("needs >= 4 CPUs")
	}
	before := currentCPUs(0)
	allowed := false
	for _, c := range before {
		allowed = allowed || c == 3
	}
	if !allowed {
		t.Skip("CPU3 excluded by the host cpuset; fallback is covered separately")
	}
	g := New(DefaultConfig())
	var inside []int
	var policy uint32
	err := g.Run(context.Background(), func(*Throttle) error {
		inside = currentCPUs(0)
		attr, err := unix.SchedGetAttr(0, 0)
		if err != nil {
			return err
		}
		policy = attr.Policy
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inside) != 1 || inside[0] != 3 {
		t.Fatalf("worker cpus = %v, want [3]", inside)
	}
	if policy != unix.SCHED_IDLE {
		t.Fatalf("policy = %d, want SCHED_IDLE", policy)
	}
	rep := g.LastReport()
	if !rep.SchedIdle || rep.Nice != 19 || !rep.IOIdle {
		t.Fatalf("report = %s", rep)
	}
	// 护栏不能泄漏到调用方线程。
	if after := currentCPUs(0); len(after) != len(before) {
		t.Fatalf("caller affinity changed: %v -> %v", before, after)
	}
}

func TestStrictCPURefusesWorkAfterAffinityDegradation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CPU = 1000
	cfg.StrictCPU = true
	ran := false
	err := New(cfg).Run(context.Background(), func(*Throttle) error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("unsafe work ran: ran=%v err=%v", ran, err)
	}
}

func TestRunDegradesWhenCPUMissing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CPU = 1000 // 不存在的核：模拟单核/少核设备或 CPU3 被 isolcpus 拿走。
	g := New(cfg)
	ran := false
	err := g.Run(context.Background(), func(*Throttle) error { ran = true; return nil })
	if err != nil || !ran {
		t.Fatalf("job must still run: ran=%v err=%v", ran, err)
	}
	rep := g.LastReport()
	if len(rep.Degraded) == 0 {
		t.Fatalf("expected degradation, got %s", rep)
	}
	if len(rep.CPUs) == 0 {
		t.Fatal("expected cpus to be reported")
	}
	for _, c := range rep.CPUs {
		if c == 1000 {
			t.Fatal("impossible cpu reported")
		}
	}
	if runtime.NumCPU() > 1 && len(rep.CPUs) != 1 {
		t.Fatalf("expected single fallback cpu, got %v", rep.CPUs)
	}
}

func TestDisabledGuardDoesNothing(t *testing.T) {
	g := New(Config{Disabled: true})
	if err := g.Run(context.Background(), func(*Throttle) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rep := g.LastReport(); rep.SchedIdle || rep.IOIdle {
		t.Fatalf("disabled guard applied settings: %s", rep)
	}
}

func TestApplyToProcessCoversAllThreads(t *testing.T) {
	if runtime.NumCPU() < 4 {
		t.Skip("needs >= 4 CPUs")
	}
	// 只设亲和性/nice 会影响整个测试进程，这里用子线程验证 /proc 枚举即可：
	// Disabled 模式下只读取不修改。
	reps, err := New(Config{Disabled: true}).ApplyToProcess(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) < 2 {
		t.Fatalf("expected multiple go runtime threads, got %d", len(reps))
	}
}

func TestThrottleSleepsAndCancels(t *testing.T) {
	th := NewThrottle(context.Background(), time.Millisecond, 2*time.Millisecond)
	deadline := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := th.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if th.Slept == 0 {
		t.Fatal("throttle never slept")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewThrottle(ctx, 0, 0).Tick(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancel, got %v", err)
	}
	var nilT *Throttle
	if nilT.Tick() != nil {
		t.Fatal("nil throttle must be no-op")
	}
}

func TestApplyToProcessHonoursStrictCPU(t *testing.T) {
	// A child process, so the test binary's own affinity stays untouched.
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Skip(err)
	}
	defer child.Process.Kill()
	cfg := DefaultConfig()
	cfg.CPU = 1000
	cfg.StrictCPU = true
	if _, err := New(cfg).ApplyToProcess(child.Process.Pid); err == nil {
		t.Fatal("StrictCPU accepted a thread off the dedicated CPU")
	}
}
