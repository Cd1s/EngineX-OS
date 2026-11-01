// Package resourceguard 把后台下载/分析工作隔离到 SC5000 的 CPU3 上，
// 以 SCHED_IDLE + nice 19 + IOPRIO_CLASS_IDLE 运行，并提供节流睡眠，
// 保证前台 CPU0（ALSA 实时音频）与 CPU1-2（Qt UI / 波形渲染）不受干扰。
//
// 所有系统调用失败都只会「降级」而不会让任务失败：拿不到 CPU3 就退到
// 最后一个在线核，没有权限设 SCHED_IDLE 就只设 nice，以此类推。
package resourceguard

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Config 描述后台工作线程的资源护栏。
type Config struct {
	// StrictCPU refuses the job if the requested dedicated CPU cannot be used.
	StrictCPU bool
	// Disabled 为 true 时不做任何系统调用（开发机调试用）。
	Disabled bool
	// CPU 是隔离核编号，SC5000 (RK3288) 上默认 3。
	CPU int
	// SchedIdle 设置 SCHED_IDLE 调度策略。
	SchedIdle bool
	// Nice 值（0..19），默认 19。
	Nice int
	// IOIdle 设置 IOPRIO_CLASS_IDLE。
	IOIdle bool
	// ThrottleWork/ThrottleSleep：每连续工作 ThrottleWork 就睡 ThrottleSleep。
	// 任一为 0 表示不节流。
	ThrottleWork  time.Duration
	ThrottleSleep time.Duration
}

// DefaultConfig 是 SC5000 上的推荐配置：CPU3 / SCHED_IDLE / nice 19 / IO idle，
// 并以约 80% 占空比节流，给 CPU3 上的内核软中断留出余量。
func DefaultConfig() Config {
	return Config{
		CPU:           3,
		SchedIdle:     true,
		Nice:          19,
		IOIdle:        true,
		ThrottleWork:  40 * time.Millisecond,
		ThrottleSleep: 10 * time.Millisecond,
	}
}

// Report 记录某个线程/进程实际生效的护栏，以及发生的降级。
type Report struct {
	TID       int      `json:"tid"`
	CPUs      []int    `json:"cpus"`
	SchedIdle bool     `json:"schedIdle"`
	Nice      int      `json:"nice"`
	IOIdle    bool     `json:"ioIdle"`
	Degraded  []string `json:"degraded,omitempty"`
}

func (r Report) String() string {
	s := fmt.Sprintf("tid=%d cpus=%v sched_idle=%v nice=%d io_idle=%v", r.TID, r.CPUs, r.SchedIdle, r.Nice, r.IOIdle)
	if len(r.Degraded) > 0 {
		s += " degraded=[" + strings.Join(r.Degraded, "; ") + "]"
	}
	return s
}

// Guard 在独立的、锁定 OS 线程的 goroutine 中执行工作函数。
type Guard struct {
	cfg Config
	mu  sync.Mutex
	// last 保存最近一次 Run 的护栏报告，供 Web UI 展示。
	last Report
}

// New 创建 Guard。
func New(cfg Config) *Guard { return &Guard{cfg: cfg} }

// Config 返回当前配置。
func (g *Guard) Config() Config { return g.cfg }

// LastReport 返回最近一次 Run 的护栏报告。
func (g *Guard) LastReport() Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// Run 在一个新的、专属的 OS 线程上执行 fn。线程在 fn 返回后随 goroutine 一起
// 退出（不调用 UnlockOSThread），因此亲和性/调度策略不会泄漏给其它 goroutine。
//
// 注意：fn 内部再起的 goroutine 不保证落在同一线程上，重活必须在 fn 自身里做。
func (g *Guard) Run(ctx context.Context, fn func(t *Throttle) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// 故意不 Unlock：让 runtime 在 goroutine 结束时销毁这个线程。
		rep := applyCurrentThread(g.cfg)
		g.mu.Lock()
		g.last = rep
		g.mu.Unlock()
		if g.cfg.StrictCPU && (len(rep.CPUs) != 1 || rep.CPUs[0] != g.cfg.CPU) {
			done <- fmt.Errorf("resourceguard: dedicated CPU%d unavailable: %s", g.cfg.CPU, rep.String())
			return
		}
		done <- fn(NewThrottle(ctx, g.cfg.ThrottleWork, g.cfg.ThrottleSleep))
	}()
	return <-done
}

// ApplyToProcess 把护栏应用到 pid 的所有线程（例如本地 Deemix 进程）。
// pid 为 0 表示当前进程。
func (g *Guard) ApplyToProcess(pid int) ([]Report, error) {
	return applyProcess(g.cfg, pid)
}

// Throttle 是协作式节流器：工作循环里频繁调用 Tick，
// 累计工作时间达到阈值后就睡一会儿，并检查 ctx 取消。
type Throttle struct {
	ctx   context.Context
	work  time.Duration
	sleep time.Duration
	start time.Time
	// Slept 累计节流睡眠时长（用于测试与诊断）。
	Slept time.Duration
}

// NewThrottle 创建节流器；work 或 sleep 为 0 时只检查取消。
func NewThrottle(ctx context.Context, work, sleep time.Duration) *Throttle {
	if ctx == nil {
		ctx = context.Background()
	}
	return &Throttle{ctx: ctx, work: work, sleep: sleep, start: time.Now()}
}

// Tick 检查取消并按需睡眠。nil 接收者合法（不节流）。
func (t *Throttle) Tick() error {
	if t == nil {
		return nil
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if t.work <= 0 || t.sleep <= 0 || time.Since(t.start) < t.work {
		return nil
	}
	timer := time.NewTimer(t.sleep)
	defer timer.Stop()
	select {
	case <-t.ctx.Done():
		return t.ctx.Err()
	case <-timer.C:
	}
	t.Slept += t.sleep
	t.start = time.Now()
	return nil
}
