package stems

// LocalRender answers Engine's Stems child protocol on the deck itself, for a
// deck with no renderer set up, with the separator in separate.go. The
// separator looks a few hundred milliseconds ahead, so the first blocks are
// answered with no frames and the rest follows after done, in pieces of at
// most maxBlockSize frames (as the network pipeline and worker.py answer).

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

func readHeader(r *bufio.Reader) (map[string]string, error) {
	f, used := map[string]string{}, 0
	for {
		line, err := r.ReadString('\n')
		if used += len(line); used > maxHeader {
			return nil, errors.New("stems: header too large")
		}
		if err != nil {
			if err == io.EOF && len(f) == 0 && line == "" {
				return nil, io.EOF
			}
			return nil, errors.New("stems: truncated header")
		}
		if line = strings.TrimRight(line, "\r\n"); line == "" {
			if len(f) == 0 {
				continue
			}
			return f, nil
		}
		k, v, ok := strings.Cut(line, "=")
		if _, dup := f[k]; !ok || k == "" || dup {
			return nil, fmt.Errorf("stems: bad header line %q", line)
		}
		f[k] = v
	}
}

func LocalRender(in io.Reader, out io.Writer) error {
	r, w := bufio.NewReaderSize(in, 64<<10), bufio.NewWriterSize(out, 64<<10)
	send := func(s string) error {
		w.WriteString(s)
		return w.Flush()
	}
	if err := send("session=FFFFFFFFFFFFFFFF\n\n"); err != nil {
		return err
	}
	init, err := readHeader(r)
	if err == io.EOF {
		return nil // a presence check: greeting only
	}
	if err != nil {
		return err
	}
	ch, _ := strconv.Atoi(init["numChannels"])
	rate, _ := strconv.ParseFloat(init["sampleRate"], 64)
	maxBlock, _ := strconv.Atoi(init["maxBlockSize"])
	if ch < 1 || ch > 2 || !(rate >= 8000 && rate <= 192000) || maxBlock < 1 || maxBlock > 65536 {
		return fmt.Errorf("stems: unsupported init %v", init)
	}
	if err = send(fmt.Sprintf("preparing=ready\n\npreparing=ready\nnumReturnChannels=%d\n\n", 4*ch)); err != nil {
		return err
	}
	sep := newSeparator(ch, rate)
	pcm := make([]byte, maxBlock*ch*4)
	// reply hands Engine the queued frames, at most maxBlock of them.
	reply := func() error {
		n := min(maxBlock, len(sep.out[0]))
		w.WriteString("returnFrames=" + strconv.Itoa(n) + "\n\n")
		for i, plane := range sep.out {
			for k, v := range plane[:n] {
				binary.LittleEndian.PutUint32(pcm[k*4:], math.Float32bits(v))
			}
			w.Write(pcm[:n*4])
			sep.out[i] = plane[n:]
		}
		return w.Flush()
	}
	for {
		f, err := readHeader(r)
		if err != nil {
			return err
		}
		switch f["action"] {
		case "done":
			sep.drain()
			for len(sep.out[0]) > 0 {
				if err = reply(); err != nil {
					return err
				}
			}
			return send("returnFrames=0\n\n")
		case "block":
		default:
			return fmt.Errorf("stems: unsupported action %q", f["action"])
		}
		n, err := strconv.Atoi(f["numSamples"])
		if err != nil || n < 0 || n > maxBlock {
			return fmt.Errorf("stems: bad numSamples %q", f["numSamples"])
		}
		x := pcm[:n*ch*4]
		if _, err = io.ReadFull(r, x); err != nil {
			return errors.New("stems: truncated PCM block")
		}
		for c := range sep.in {
			for k := 0; k < n; k++ {
				v := binary.LittleEndian.Uint32(x[(c*n+k)*4:])
				if v&0x7f800000 == 0x7f800000 { // NaN or infinite
					return errors.New("stems: non-finite PCM")
				}
				sep.in[c] = append(sep.in[c], math.Float32frombits(v))
			}
		}
		sep.owed += n
		sep.run()
		if err = reply(); err != nil {
			return err
		}
	}
}
