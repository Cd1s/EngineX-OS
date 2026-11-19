// Package testsmb runs an authenticated Samba fixture when smbd is available.
package testsmb

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type Server struct {
	Host, User, Password, URL, Root string
	Port                            int
	cmd                             *exec.Cmd
}

func Start(t *testing.T) *Server {
	t.Helper()
	smbd, err := exec.LookPath("smbd")
	if err != nil {
		t.Skip("Samba integration fixture requires smbd and smbpasswd")
	}
	dir := t.TempDir()
	s := &Server{Host: "127.0.0.1", User: "root", Password: "fixture-password", Root: filepath.Join(dir, "music")}
	os.MkdirAll(s.Root, 0700)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Port = l.Addr().(*net.TCPAddr).Port
	l.Close()
	s.URL = "smb://" + net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) + "/music"
	conf := filepath.Join(dir, "smb.conf")
	config := fmt.Sprintf(`[global]
server role = standalone server
security = user
map to guest = Never
server min protocol = SMB3_00
interfaces = 127.0.0.1
bind interfaces only = yes
disable netbios = yes
smb ports = %d
private dir = %s
state directory = %s
cache directory = %s
lock directory = %s
pid directory = %s
ncalrpc dir = %s
log file = %s/log
load printers = no
printing = bsd
printcap name = /dev/null
[music]
path = %s
read only = yes
valid users = root
`, s.Port, dir, dir, dir, dir, dir, dir, dir, s.Root)
	if err := os.WriteFile(conf, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	add := exec.Command("smbpasswd", "-s", "-a", "root", "-c", conf)
	add.Stdin = strings.NewReader(s.Password + "\n" + s.Password + "\n")
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("smbpasswd: %v: %s", err, out)
	}
	log, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	s.cmd = exec.Command(smbd, "--foreground", "--no-process-group", "--configfile="+conf, "--debug-stdout", "--debuglevel=1")
	s.cmd.Stdout, s.cmd.Stderr = log, log
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(); s.cmd.Wait(); log.Close() })
	for start := time.Now(); time.Since(start) < 5*time.Second; {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), 50*time.Millisecond)
		if err == nil {
			c.Close()
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := os.ReadFile(log.Name())
	t.Fatalf("Samba did not listen: %s", out)
	return nil
}

func (s *Server) Stop() { _ = s.cmd.Process.Signal(os.Interrupt) }

// SlowProxy keeps a copy in flight long enough to test readers and disconnections.
func (s *Server) SlowProxy(t *testing.T) int {
	port, _ := s.slowProxy(t, false)
	return port
}

// SlowProxyHold pauses the first large response after the SMB handshake until
// the caller releases it, making an in-flight copy assertion deterministic.
func (s *Server) SlowProxyHold(t *testing.T) (int, func()) {
	return s.slowProxy(t, true)
}

func (s *Server) slowProxy(t *testing.T, hold bool) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var releaseOnce sync.Once
	released := make(chan struct{})
	release := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(release)
	if !hold {
		release()
	}
	go func() {
		for {
			client, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				server, err := net.DialTimeout("tcp", net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), time.Second)
				if err != nil {
					return
				}
				defer server.Close()
				done := make(chan struct{})
				go func() { io.Copy(server, client); server.Close(); close(done) }()
				buf := make([]byte, 32<<10)
				var sent int64
				for {
					n, err := server.Read(buf)
					if n > 0 {
						time.Sleep(time.Millisecond)
						if hold && sent < 1<<20 && sent+int64(n) > 1<<20 {
							cut := int((1 << 20) - sent)
							if _, e := client.Write(buf[:cut]); e != nil {
								break
							}
							<-released
							if _, e := client.Write(buf[cut:n]); e != nil {
								break
							}
						} else if _, e := client.Write(buf[:n]); e != nil {
							break
						}
						sent += int64(n)
					}
					if err != nil {
						break
					}
				}
				client.Close()
				<-done
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, release
}
