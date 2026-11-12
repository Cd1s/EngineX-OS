package dbhealer

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// killedHealEnv names the m.db a child of TestHealKilledMidTransaction writes to.
const killedHealEnv = "DBHEALER_KILLED_HEAL_DB"

// TestKilledHealChild is the heal killed at the mount deadline: it opens the library as the
// heal does, lets a large transaction spill pages into m.db, says so and waits to be killed.
func TestKilledHealChild(t *testing.T) {
	path := os.Getenv(killedHealEnv)
	if path == "" {
		t.Skip("child of TestHealKilledMidTransaction")
	}
	db, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"PRAGMA cache_size=1", "PRAGMA synchronous=NORMAL", "BEGIN", "UPDATE Track SET path=path||'-half-written'"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("in-transaction")
	time.Sleep(time.Minute)
}

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// BH2-A1-3: the pre-mount heal of a writable stick is SIGKILLed when the mount deadline comes.
// What it leaves on the stick is what any killed SQLite writer leaves: a hot rollback journal
// and pages of the unfinished transaction in m.db. The next writable open of the library,
// Engine's on the visible mount or the next heal, rolls it back to the last commit.
func TestHealKilledMidTransaction(t *testing.T) {
	for _, next := range []string{"engine", "heal"} {
		t.Run(next, func(t *testing.T) {
			mount := t.TempDir()
			dir := filepath.Join(mount, "Engine Library", "Database2")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			db, src := newLibrary(t)
			pad := strings.Repeat("p", 200)
			for i := 1; i <= 1500; i++ {
				mustExec(t, db, "INSERT INTO Track(id,path,isAnalyzed) VALUES(?,?,1)", i, fmt.Sprintf("Music/%s/%d.mp3", pad, i))
			}
			db.Close()
			path := filepath.Join(dir, "m.db")
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o644); err != nil {
				t.Fatal(err)
			}
			before := hashFile(t, path)

			cmd := exec.Command(os.Args[0], "-test.run=^TestKilledHealChild$", "-test.count=1")
			cmd.Env = append(os.Environ(), killedHealEnv+"="+path)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := make(chan bool, 1)
			go func() {
				s := bufio.NewScanner(out)
				for s.Scan() {
					if s.Text() == "in-transaction" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					cmd.Process.Kill()
					cmd.Wait()
					t.Fatal("the child ended before its transaction")
				}
			case <-time.After(30 * time.Second):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("the child did not reach its transaction")
			}
			cmd.Process.Signal(syscall.SIGKILL)
			cmd.Wait()

			// What the deadline leaves: a hot journal and a half-written m.db.
			if st, err := os.Stat(path + "-journal"); err != nil || st.Size() == 0 {
				t.Fatalf("no hot journal after the kill (%v)", err)
			}
			if hashFile(t, path) == before {
				t.Fatal("m.db was not written before the kill: the fixture does not reach the half-written state")
			}
			// A read-only reader is told to recover on writable media (dbhealer.Open).
			if _, err := Open(path, true); err == nil || !strings.Contains(err.Error(), "-journal") {
				t.Fatalf("read-only open of a hot journal: %v", err)
			}

			switch next {
			case "engine": // Engine's own open of m.db on the writable visible mount
				e, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				var n int
				if err := e.QueryRow("SELECT count(*) FROM Track").Scan(&n); err != nil || n != 1500 {
					t.Fatalf("Engine's open: %d tracks, %v", n, err)
				}
				e.Close()
			case "heal": // the next insert's heal
				if r, err := HealMountDataOnly(mount, 1); err != nil {
					t.Fatalf("heal after a killed heal: %+v %v", r, err)
				}
			}
			check, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer check.Close()
			var integrity string
			var half, n int
			if err := check.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity after recovery: %q %v", integrity, err)
			}
			if err := check.QueryRow("SELECT count(*), sum(path LIKE '%-half-written') FROM Track").Scan(&n, &half); err != nil || n != 1500 || half != 0 {
				t.Fatalf("after recovery: %d tracks, %d half-written, %v", n, half, err)
			}
			if st, err := os.Stat(path + "-journal"); err == nil && st.Size() > 0 {
				t.Fatal("the journal is still hot after the writable open")
			}
		})
	}
}
