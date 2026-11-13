package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestFakeStemsChildWithInfoHeaders(t *testing.T) {
	if os.Args[len(os.Args)-1] != "fake-stems-child" {
		return
	}
	in, out := bufio.NewReader(os.Stdin), bufio.NewWriter(os.Stdout)
	send := func(s string) { out.WriteString(s); out.Flush() }
	readHdr := func() map[string]string {
		f := map[string]string{}
		for {
			l, err := in.ReadString('\n')
			if err != nil {
				os.Exit(2)
			}
			if l = strings.TrimSpace(l); l == "" {
				if len(f) == 0 {
					continue
				}
				return f
			}
			k, v, _ := strings.Cut(l, "=")
			f[k] = v
		}
	}
	send("session=FFFFFFFFFFFFFFFF\n\n")
	init := readHdr()
	ch, _ := strconv.Atoi(init["numChannels"])
	send(fmt.Sprintf("preparing=ready\n\npreparing=ready\nnumReturnChannels=%d\n\n", 4*ch))
	for {
		f := readHdr()
		if f["action"] == "done" {
			break
		}
		n, _ := strconv.Atoi(f["numSamples"])
		if _, err := io.ReadFull(in, make([]byte, n*ch*4)); err != nil {
			os.Exit(3)
		}
		// Informational non-frame header mid-session before returnFrames.
		send("status=separating\n\n")
		out.WriteString(fmt.Sprintf("returnFrames=%d\n\n", n))
		out.Write(make([]byte, n*4*ch*4))
		out.Flush()
	}
	// Informational non-frame header during drain before final returnFrames=0.
	send("status=draining\n\n")
	send("returnFrames=0\n\n")
	os.Exit(0)
}

func TestProbeSkipsInformationalHeaders(t *testing.T) {
	var report bytes.Buffer
	err := runProbe(os.Args[0], []string{"-test.run=^TestFakeStemsChildWithInfoHeaders$", "--", "fake-stems-child"}, 0.05, 256, &report)
	if err != nil {
		t.Fatalf("runProbe failed on informational headers: %v (report=%s)", err, report.String())
	}
	if !strings.Contains(report.String(), `"protocol":"ok"`) {
		t.Fatalf("unexpected report: %s", report.String())
	}
}
