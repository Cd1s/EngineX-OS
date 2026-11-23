package bridge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func appendHelperProbeLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(line + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInChildForTestPattern(t *testing.T) {
	marker := os.Getenv("BRIDGE_HELPER_PATTERN_MARKER")
	if marker == "" {
		marker = filepath.Join(t.TempDir(), "pattern.txt")
		cmd := exec.Command(os.Args[0], "-test.run=^TestInChildForTestPattern$", "-test.v")
		cmd.Env = append(os.Environ(), "BRIDGE_HELPER_PATTERN_MARKER="+marker)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pattern probe: %v\n%s", err, out)
		}
		b, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Fields(string(b))
		if len(lines) != 1 || lines[0] != "TestInChildForTestPattern/mid/leaf" {
			t.Fatalf("child ran %v, want only the named subtest", lines)
		}
		return
	}

	t.Run("mid", func(t *testing.T) {
		t.Run("leaf", func(t *testing.T) {
			if InChildForTest(t) {
				return
			}
			appendHelperProbeLine(t, marker, t.Name())
		})
		t.Run("xmidy", func(t *testing.T) {
			t.Run("xleaf", func(t *testing.T) {
				if os.Getenv("BRIDGE_TEST_CHILD") != "" {
					appendHelperProbeLine(t, marker, "unexpected:"+t.Name())
				}
			})
		})
	})
}

func TestInChildForTestPatternExtra(t *testing.T) {
	if marker := os.Getenv("BRIDGE_HELPER_PATTERN_MARKER"); marker != "" && os.Getenv("BRIDGE_TEST_CHILD") != "" {
		appendHelperProbeLine(t, marker, "unexpected:"+t.Name())
	}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func TestInChildForTestTimeout(t *testing.T) {
	if os.Getenv("BRIDGE_HELPER_TIMEOUT_PROBE") == "" {
		pidFile := filepath.Join(t.TempDir(), "pid")
		cmd := exec.Command(os.Args[0], "-test.run=^TestInChildForTestTimeout$", "-test.timeout=400ms", "-test.v")
		cmd.Env = append(os.Environ(),
			"BRIDGE_HELPER_TIMEOUT_PROBE=1",
			"BRIDGE_HELPER_TIMEOUT_PID="+pidFile,
		)
		started := time.Now()
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("timeout probe passed unexpectedly\n%s", out)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("timeout probe took %s\n%s", elapsed, out)
		}
		b, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			t.Fatalf("read child pid: %v\n%s", readErr, out)
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(b)))
		if parseErr != nil {
			t.Fatalf("parse child pid: %v", parseErr)
		}
		defer func() {
			if processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}()
		time.Sleep(100 * time.Millisecond)
		if processAlive(pid) {
			t.Fatalf("child process %d survived its parent\n%s", pid, out)
		}
		return
	}

	if InChildForTest(t) {
		return
	}
	if err := os.WriteFile(os.Getenv("BRIDGE_HELPER_TIMEOUT_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	t.Fatal("child did not time out")
}

func TestInChildForTestSkipVisibility(t *testing.T) {
	if os.Getenv("BRIDGE_HELPER_SKIP_PROBE") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestInChildForTestSkipVisibility$", "-test.v")
		cmd.Env = append(os.Environ(), "BRIDGE_HELPER_SKIP_PROBE=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("skip probe: %v\n%s", err, out)
		}
		for _, name := range []string{
			"TestInChildForTestSkipVisibility/target/skip_one",
			"TestInChildForTestSkipVisibility/target/skip_two",
			"TestInChildForTestSkipVisibility/top_skip",
		} {
			if !strings.Contains(string(out), name) {
				t.Fatalf("skip output lacks %q\n%s", name, out)
			}
		}
		return
	}

	t.Run("target", func(t *testing.T) {
		if InChildForTest(t) {
			return
		}
		t.Run("skip_one", func(t *testing.T) { t.Skip("probe skip one") })
		t.Run("skip_two", func(t *testing.T) { t.Skip("probe skip two") })
	})
	t.Run("top_skip", func(t *testing.T) {
		if InChildForTest(t) {
			return
		}
		t.Skip("probe top skip")
	})
}
