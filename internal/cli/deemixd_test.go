package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHelperDeemixd is the child the supervisor starts: it listens like
// sc5000-deemixd until asked to exit.
func TestHelperDeemixd(t *testing.T) {
	addr := os.Getenv("CLI_HELPER_ADDR")
	if addr == "" {
		t.Skip("helper process")
	}
	if mark := os.Getenv("CLI_HELPER_MARK"); mark != "" {
		f, _ := os.OpenFile(mark, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString("start\n")
		f.Close()
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(3)
	}
	http.Serve(l, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { os.Exit(0) }))
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func shortPauses(t *testing.T) {
	p, b, m := deemixdPause, deemixdMaxBackoff, deemixdMissing
	deemixdPause, deemixdMaxBackoff, deemixdMissing = 20*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { deemixdPause, deemixdMaxBackoff, deemixdMissing = p, b, m })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
	}
}

func starts(mark string) int {
	b, _ := os.ReadFile(mark)
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func TestSuperviseDeemixdStartsAndRestarts(t *testing.T) {
	shortPauses(t)
	addr := freeAddr(t)
	mark := filepath.Join(t.TempDir(), "starts")
	t.Setenv("CLI_HELPER_ADDR", addr)
	t.Setenv("CLI_HELPER_MARK", mark)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { superviseDeemixd(ctx, addr, os.Args[0], "-test.run=^TestHelperDeemixd$"); close(done) }()
	waitFor(t, "downloader never came up", func() bool { return listening(addr) })
	http.Get("http://" + addr + "/") // the helper exits
	waitFor(t, "downloader was not restarted", func() bool { return starts(mark) >= 2 && listening(addr) })
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor kept running after cancel")
	}
	waitFor(t, "child outlived the supervisor", func() bool { return !listening(addr) })
}

func TestSuperviseDeemixdLeavesARunningCopyAlone(t *testing.T) {
	shortPauses(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go http.Serve(l, http.NotFoundHandler())
	mark := filepath.Join(t.TempDir(), "starts")
	t.Setenv("CLI_HELPER_ADDR", l.Addr().String())
	t.Setenv("CLI_HELPER_MARK", mark)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	superviseDeemixd(ctx, l.Addr().String(), os.Args[0], "-test.run=^TestHelperDeemixd$")
	if n := starts(mark); n != 0 {
		t.Fatalf("started %d copies beside a running one", n)
	}
	// A missing binary only waits.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	superviseDeemixd(ctx, freeAddr(t), filepath.Join(t.TempDir(), "missing"))
}

func TestSameDeemixd(t *testing.T) {
	for base, want := range map[string]bool{deemixdURL: true, "http://localhost:6595/": true, "http://10.0.0.2:6595": false, "http://127.0.0.1:7000": false, "%": false} {
		if sameDeemixd(base) != want {
			t.Fatal(base)
		}
	}
}
