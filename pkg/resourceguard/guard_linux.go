//go:build linux

package resourceguard

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

const (
	ioprioClassShift = 13
	ioprioClassIdle  = 3
	ioprioWhoProcess = 1
	// maxCPUs 与 unix.CPUSet 的位宽一致（1024）。
	maxCPUs = 1024
)

func applyCurrentThread(cfg Config) Report {
	return applyTID(cfg, unix.Gettid())
}

func applyProcess(cfg Config, pid int) ([]Report, error) {
	if pid == 0 {
		pid = os.Getpid()
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	var reps []Report
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		reps = append(reps, applyTID(cfg, tid))
	}
	return reps, nil
}

// applyTID 对单个线程设置亲和性、调度策略、nice 与 IO 优先级。
// 在 Linux 上这些系统调用以 tid 为粒度生效，新建线程会继承它们。
func applyTID(cfg Config, tid int) Report {
	rep := Report{TID: tid, Nice: -1}
	if cfg.Disabled {
		rep.Degraded = append(rep.Degraded, "resource guard disabled")
		rep.CPUs = currentCPUs(tid)
		return rep
	}

	// 1. CPU 亲和性：优先 cfg.CPU，失败则降级到最后一个可用核，避开 CPU0。
	if err := setAffinity(tid, cfg.CPU); err != nil {
		fallback := fallbackCPU(tid, cfg.CPU)
		msg := fmt.Sprintf("cpu%d unavailable (%v)", cfg.CPU, err)
		if fallback >= 0 && setAffinity(tid, fallback) == nil {
			msg += fmt.Sprintf(", fell back to cpu%d", fallback)
		} else {
			msg += ", affinity unchanged"
		}
		rep.Degraded = append(rep.Degraded, msg)
	}
	rep.CPUs = currentCPUs(tid)

	// 2. SCHED_IDLE（同时带上 nice，sched_setattr 一次设好）。
	if cfg.SchedIdle {
		attr := &unix.SchedAttr{Size: unix.SizeofSchedAttr, Policy: unix.SCHED_IDLE, Nice: int32(cfg.Nice)}
		if err := unix.SchedSetAttr(tid, attr, 0); err != nil {
			rep.Degraded = append(rep.Degraded, fmt.Sprintf("SCHED_IDLE failed (%v)", err))
		} else {
			rep.SchedIdle = true
		}
	}

	// 3. nice：sched_setattr 失败或未启用 SCHED_IDLE 时单独设一次。
	if err := unix.Setpriority(unix.PRIO_PROCESS, tid, cfg.Nice); err != nil {
		rep.Degraded = append(rep.Degraded, fmt.Sprintf("nice %d failed (%v)", cfg.Nice, err))
	}
	if p, err := unix.Getpriority(unix.PRIO_PROCESS, tid); err == nil {
		// 内核返回 20-nice。
		rep.Nice = 20 - p
	}

	// 4. IOPRIO_CLASS_IDLE。
	if cfg.IOIdle {
		prio := ioprioClassIdle << ioprioClassShift
		if _, _, errno := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(prio)); errno != 0 {
			rep.Degraded = append(rep.Degraded, fmt.Sprintf("ioprio idle failed (%v)", errno))
		} else {
			rep.IOIdle = true
		}
	}
	return rep
}

func setAffinity(tid, cpu int) error {
	if cpu < 0 || cpu >= maxCPUs {
		return fmt.Errorf("cpu index %d out of range", cpu)
	}
	var set unix.CPUSet
	set.Set(cpu)
	return unix.SchedSetaffinity(tid, &set)
}

// fallbackCPU 返回当前允许集合里编号最大的核（不等于 want），没有则 -1。
// 单核机器上返回 -1，即保持原亲和性。
func fallbackCPU(tid, want int) int {
	cpus := currentCPUs(tid)
	for i := len(cpus) - 1; i >= 0; i-- {
		if cpus[i] != want && cpus[i] != 0 {
			return cpus[i]
		}
	}
	return -1
}

func currentCPUs(tid int) []int {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(tid, &set); err != nil {
		return nil
	}
	var cpus []int
	for i := 0; i < maxCPUs && len(cpus) < set.Count(); i++ {
		if set.IsSet(i) {
			cpus = append(cpus, i)
		}
	}
	return cpus
}
