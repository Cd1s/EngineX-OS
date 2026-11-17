package webui

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHGen2PasswordOnReadOnlyEtc299(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("needs openssl")
	}
	for _, board := range []string{"inmusic,jp21", "inmusic,jp20", "inmusic,jc11s", "inmusic,jp11s", "inmusic,nh08s", "inmusic,nh10", "inmusic,jp24", "inmusic,rmz2"} {
		t.Run(board, func(t *testing.T) {
			asDeck(t, board)
			dir := t.TempDir()
			oldPort, oldRun := sshPortDropInPath, sshRunDropInPath
			blocked := filepath.Join(dir, "readonly-etc")
			if err := os.WriteFile(blocked, nil, 0400); err != nil {
				t.Fatal(err)
			}
			sshPortDropInPath = filepath.Join(blocked, "port.conf")
			sshRunDropInPath = filepath.Join(dir, "run-port.conf")
			defer func() { sshPortDropInPath, sshRunDropInPath = oldPort, oldRun }()
			source := filepath.Join(dir, "shadow")
			before := "root:!:20000:0:99999:7:::\nother:*:20000:0:99999:7:::\n"
			if err := os.WriteFile(source, []byte(before), 0400); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			os.Mkdir(bin, 0700)
			if err := os.WriteFile(filepath.Join(bin, "mount"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			u := &unitRunner{state: "active"}
			SetSystemRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "sh" && len(args) > 1 {
					if strings.Contains(args[1], "chpasswd") {
						return nil, errors.New("read-only /etc")
					}
					if strings.Contains(args[1], "openssl passwd") || strings.Contains(args[1], "cmp -s") {
						for i, arg := range args {
							if arg == "/etc/shadow" {
								args[i] = source
							}
						}
						return exec.CommandContext(ctx, name, args...).CombinedOutput()
					}
				}
				return u.run(ctx, name, args...)
			})
			defer SetSystemRunner(nil)
			state := filepath.Join(dir, "state")
			os.Mkdir(state, 0700)
			s := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
			defer s.Close()
			if w := sshCall(s, http.MethodPost, `{"password":"test-only"}`); w.Code != 200 {
				t.Fatalf("password save: HTTP %d", w.Code)
			}
			store := filepath.Join(state, "ssh-shadow")
			b, err := os.ReadFile(store)
			if err != nil || !strings.HasPrefix(string(b), "root:$6$") || !strings.Contains(string(b), "other:*:20000:0:99999:7:::") || strings.Contains(string(b), "test-only") {
				t.Fatal("stored shadow is missing, malformed, or contains plaintext")
			}
			if st, err := os.Stat(store); err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("shadow permissions")
			}
			if b, _ := os.ReadFile(source); string(b) != before {
				t.Fatal("wrote the read-only source")
			}
			drop, _ := os.ReadFile(sshRunDropInPath)
			for _, want := range []string{"-p 22", "-o PasswordAuthentication=yes", "-o PermitRootLogin=yes"} {
				if !strings.Contains(string(drop), want) {
					t.Errorf("missing sshd option %s", want)
				}
			}
			if !u.ran("systemctl restart sshd") {
				t.Fatal("password did not activate SSH")
			}
			if w := sshCall(s, http.MethodGet, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"hasPassword":true`) || strings.Contains(w.Body.String(), "test-only") || strings.Contains(w.Body.String(), "$6$") {
				t.Fatal("password status or redaction")
			}
			// Re-enable with saved credentials, without resubmitting the password.
			u.cmds = nil
			if w := sshCall(s, http.MethodPost, `{"enabled":true,"port":2223}`); w.Code != 200 {
				t.Fatal("saved password could not be re-enabled")
			}
			drop, _ = os.ReadFile(sshRunDropInPath)
			if !strings.Contains(string(drop), "PasswordAuthentication=yes") || !strings.Contains(string(drop), "-p 2223") {
				t.Fatal("port change lost password auth")
			}
			if w := sshCall(s, http.MethodPost, `{"authorizedKey":"ssh-ed25519 AAAA test"}`); w.Code != 200 {
				t.Fatal("key upload stopped working")
			}
		})
	}
}

func TestSSHPasswordFailureDoesNotEnable299(t *testing.T) {
	for _, stage := range []string{"openssl passwd", "cmp -s"} {
		t.Run(stage, func(t *testing.T) {
			asDeck(t, "inmusic,jp21")
			state := t.TempDir()
			if err := os.WriteFile(filepath.Join(state, "ssh-shadow"), []byte("root:$6$fixture:20000:0:99999:7:::\n"), 0600); err != nil {
				t.Fatal(err)
			}
			u := &unitRunner{state: "inactive"}
			SetSystemRunner(func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "sh" && len(args) > 1 && strings.Contains(args[1], stage) {
					return []byte("private-process-output"), errors.New("fixture failure")
				}
				return u.run(ctx, name, args...)
			})
			defer SetSystemRunner(nil)
			s := New(Config{StateDir: state, UIDir: t.TempDir()}, nil)
			defer s.Close()
			w := sshCall(s, http.MethodPost, `{"password":"test-only"}`)
			if w.Code != 500 || strings.Contains(w.Body.String(), "private-process-output") || strings.Contains(w.Body.String(), "test-only") {
				t.Fatal("failed password operation reported success or exposed input/output")
			}
			if s.loadSSHConfig().HasPassword || u.ran("systemctl restart sshd") || u.ran("systemctl start sshd") {
				t.Fatal("failed password operation enabled SSH")
			}
		})
	}
}
