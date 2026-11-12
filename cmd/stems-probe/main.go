// Command stems-probe plays Engine's side of a Stems render against a client
// such as stems-processor: init, block by block (waiting for each reply, as
// tools/stems/render.py does), done, then it keeps the child's stdin open
// the way Engine may and times how long the child takes to exit. The audio
// is a synthetic stereo tone; output is checked with stems.Session.
//
//	stems-probe -seconds 20 -block 1024 /usr/local/bin/stems-processor
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Cd1s/EngineX-OS/internal/stems"
)

func main() {
	seconds := flag.Float64("seconds", 20, "audio length")
	block := flag.Int("block", 1024, "frames per block (Engine's maxBlockSize)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: stems-probe [-seconds N] [-block N] <client>")
		os.Exit(2)
	}
	if err := runProbe(flag.Arg(0), []string{"-s", "-hv", "1.0.0", "-pv", "1.6.0"}, *seconds, *block, os.Stdout); err != nil {
		fail(err)
	}
}

func runProbe(bin string, args []string, seconds float64, block int, report io.Writer) error {
	const rate, channels = 44100, 2
	cmd := exec.Command(bin, args...)
	cmd.Stderr = os.Stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	sess := &stems.Session{}
	in := io.MultiWriter(stdin, writerFunc(sess.FeedIn))
	out := bufio.NewReader(io.TeeReader(stdout, writerFunc(sess.FeedOut)))
	header := func() (map[string]string, error) {
		f := map[string]string{}
		for {
			l, err := out.ReadString('\n')
			if err != nil {
				return nil, fmt.Errorf("client output ended after %.1fs: %w", time.Since(start).Seconds(), err)
			}
			if l = strings.TrimRight(l, "\r\n"); l == "" {
				if len(f) == 0 {
					continue
				}
				return f, nil
			}
			k, v, _ := strings.Cut(l, "=")
			f[k] = v
		}
	}
	reply := func() (int, error) {
		for {
			h, err := header()
			if err != nil {
				return 0, err
			}
			v, ok := h["returnFrames"]
			if !ok {
				continue
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return 0, err
			}
			if _, err = io.CopyN(io.Discard, out, int64(n)*4*channels*4); err != nil {
				return 0, err
			}
			return n, nil
		}
	}
	if _, err := header(); err != nil { // session
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	fmt.Fprintf(in, "version=1.6.0\ntime=%d\nnumChannels=%d\nsampleRate=%d\nmaxBlockSize=%d\nhighQuality=1\nresidual=0\nnumThreads=1\nsession=FFFFFFFFFFFFFFFF\n\n", time.Now().Unix(), channels, rate, block)
	for {
		h, err := header()
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		if h["numReturnChannels"] != "" {
			break
		}
	}
	connected := time.Since(start)
	total := int(seconds * rate)
	pcm := make([]byte, block*channels*4)
	var wait time.Duration
	for sent := 0; sent < total; sent += block {
		n := min(block, total-sent)
		for c := 0; c < channels; c++ {
			for i := 0; i < n; i++ {
				v := float32(0.3 * math.Sin(2*math.Pi*220*float64(sent+i)/rate))
				binary.LittleEndian.PutUint32(pcm[(c*n+i)*4:], math.Float32bits(v))
			}
		}
		fmt.Fprintf(in, "action=block\nnumSamples=%d\n\n", n)
		in.Write(pcm[:n*channels*4])
		t := time.Now()
		if _, err := reply(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		wait += time.Since(t)
	}
	fmt.Fprint(in, "action=done\n\n")
	for {
		n, err := reply()
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return err
		}
		if n == 0 {
			break
		}
	}
	drained := time.Since(start)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }() // stdin deliberately left open
	var err error
	select {
	case err = <-exited:
	case <-time.After(150 * time.Second):
		cmd.Process.Kill()
		err = fmt.Errorf("child still running 150s after the final reply")
		<-exited
	}
	stdin.Close()
	status := "ok"
	if e := sess.Complete(); e != nil {
		status = e.Error()
	}
	json.NewEncoder(report).Encode(map[string]any{
		"audio_seconds": seconds, "block": block, "blocks": sess.Blocks, "frames_in": sess.FramesIn, "frames_out": sess.FramesOut,
		"connect_s": connected.Seconds(), "reply_wait_s": wait.Seconds(), "drained_s": drained.Seconds(),
		"exit_after_drain_s": (time.Since(start) - drained).Seconds(), "exit": fmt.Sprint(err), "protocol": status,
	})
	if err != nil {
		return err
	}
	if status != "ok" {
		return fmt.Errorf("protocol: %s", status)
	}
	return nil
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "stems-probe:", err)
	os.Exit(1)
}
