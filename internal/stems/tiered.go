package stems

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Official MIT Deezer Spleeter v1.4.0 4-stem archive and SHA-256.
	Spleeter4StemsURL    = "https://github.com/deezer/spleeter/releases/download/v1.4.0/4stems.tar.gz"
	Spleeter4StemsSHA256 = "3adb4a50ad4eb18c7c4d65fcf4cf2367a07d48408a5eb7d03cd20067429dfaa8"

	// Official MIT Meta HTDemucs v4 safetensors conversion and SHA-256.
	HTDemucsSafetensorsURL    = "https://huggingface.co/set-soft/audio_separation/resolve/main/Demucs/htdemucs.safetensors"
	HTDemucsSafetensorsSHA256 = "8193504cdfb3943adaf039b8acb524a46e87ebf232c383ac7a32c80a6578423e"

	hqCacheMagic = "EXHQSTM1"
)

// WholeTrackFunc separates planar [channels][frames] audio at sampleRate into
// canonical [4][channels][frames] stems: vocals, bass, drums, melody.
type WholeTrackFunc func(ctx context.Context, x [][]float32, sampleRate float64) ([][][]float32, error)

// TrackAudioDigest computes a framing-invariant SHA-256 over (channels, sampleRate,
// frames, int16-quantized PCM) so Framing:1 and Framing:2 sessions hit the same key.
func TrackAudioDigest(x [][]float32, sampleRate float64) string {
	h := sha256.New()
	var hdr [20]byte
	ch := len(x)
	frames := 0
	if ch > 0 {
		frames = len(x[0])
	}
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(ch))
	binary.LittleEndian.PutUint64(hdr[4:12], uint64(math.Round(sampleRate)))
	binary.LittleEndian.PutUint64(hdr[12:20], uint64(frames))
	h.Write(hdr[:])

	buf := make([]byte, min(frames, 4096)*2)
	for c := 0; c < ch; c++ {
		plane := x[c]
		for off := 0; off < frames; off += 4096 {
			end := min(off+4096, frames)
			cnt := end - off
			for i := 0; i < cnt; i++ {
				q := math.Round(float64(plane[off+i]) * packScale)
				q = math.Max(math.MinInt16, math.Min(math.MaxInt16, q))
				binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(q)))
			}
			h.Write(buf[:cnt*2])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HQCache stores 3-stem int16 high-quality separations keyed by TrackAudioDigest.
type HQCache struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
	seq      atomic.Uint64
}

func NewHQCache(dir string, maxMB int) (*HQCache, error) {
	if dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if maxMB <= 0 {
		maxMB = 2048
	}
	return &HQCache{dir: dir, maxBytes: int64(maxMB) << 20}, nil
}

func (c *HQCache) path(digest string) string {
	return filepath.Join(c.dir, digest+".hqstems")
}

// Get loads cached high-quality stems for digest and rebuilds melody = x - vocals - bass - drums.
func (c *HQCache) Get(digest string, x [][]float32, sampleRate float64) ([][][]float32, bool) {
	if c == nil || c.dir == "" || len(x) == 0 {
		return nil, false
	}
	p := c.path(digest)
	raw, err := os.ReadFile(p)
	if err != nil || len(raw) < 28 || string(raw[:8]) != hqCacheMagic {
		return nil, false
	}
	ch := int(binary.LittleEndian.Uint32(raw[8:12]))
	rate := math.Float64frombits(binary.LittleEndian.Uint64(raw[12:20]))
	frames := int(binary.LittleEndian.Uint64(raw[20:28]))
	if ch != len(x) || frames != len(x[0]) || math.Abs(rate-sampleRate) > 1.0 {
		return nil, false
	}
	wantPayload := 3 * ch * frames * 2
	if len(raw)-28 != wantPayload {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(p, now, now)

	payload := raw[28:]
	out := make([][][]float32, 4)
	for st := 0; st < 4; st++ {
		out[st] = make([][]float32, ch)
	}
	off := 0
	for st := 0; st < 3; st++ {
		for ci := 0; ci < ch; ci++ {
			plane := make([]float32, frames)
			for k := 0; k < frames; k++ {
				plane[k] = unpackSample(payload[off:])
				off += 2
			}
			out[st][ci] = plane
		}
	}
	for ci := 0; ci < ch; ci++ {
		mel := make([]float32, frames)
		inC := x[ci]
		vC, bC, dC := out[0][ci], out[1][ci], out[2][ci]
		for k := 0; k < frames; k++ {
			mel[k] = inC[k] - vC[k] - bC[k] - dC[k]
		}
		out[3][ci] = mel
	}
	return out, true
}

// Put writes 3-stem int16 planes atomically and evicts oldest entries above maxBytes.
func (c *HQCache) Put(digest string, stems [][][]float32, sampleRate float64) error {
	if c == nil || c.dir == "" || len(stems) < 3 || len(stems[0]) == 0 {
		return nil
	}
	ch := len(stems[0])
	frames := len(stems[0][0])
	payloadLen := 3 * ch * frames * 2
	if int64(28+payloadLen) > c.maxBytes {
		return nil
	}
	buf := make([]byte, 28+payloadLen)
	copy(buf[:8], hqCacheMagic)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(ch))
	binary.LittleEndian.PutUint64(buf[12:20], math.Float64bits(sampleRate))
	binary.LittleEndian.PutUint64(buf[20:28], uint64(frames))
	off := 28
	for st := 0; st < 3; st++ {
		for ci := 0; ci < ch; ci++ {
			plane := stems[st][ci]
			for k := 0; k < frames; k++ {
				q := math.Round(float64(plane[k]) * packScale)
				q = math.Max(math.MinInt16, math.Min(math.MaxInt16, q))
				binary.LittleEndian.PutUint16(buf[off:], uint16(int16(q)))
				off += 2
			}
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	tmp := fmt.Sprintf("%s.tmp.%d.%d", c.path(digest), os.Getpid(), c.seq.Add(1))
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.path(digest)); err != nil {
		os.Remove(tmp)
		return err
	}
	c.evictLocked()
	return nil
}

func (c *HQCache) evictLocked() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	var items []item
	var total int64
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".hqstems" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		items = append(items, item{
			path:  filepath.Join(c.dir, e.Name()),
			size:  info.Size(),
			mtime: info.ModTime(),
		})
	}
	if total <= c.maxBytes {
		return
	}
	slices.SortFunc(items, func(a, b item) int {
		return a.mtime.Compare(b.mtime)
	})
	for _, it := range items {
		if total <= c.maxBytes {
			break
		}
		if os.Remove(it.path) == nil {
			total -= it.size
		}
	}
}

type bgHQJob struct {
	digest string
	rate   float64
	mix    [][]float32
}

// TieredEngine serves instant stems (Spleeter / HPSS) on cache miss while
// computing and caching high-quality stems (HTDemucs) asynchronously in the background.
type TieredEngine struct {
	Instant WholeTrackFunc
	HQ      WholeTrackFunc
	Cache   *HQCache
	Log     io.Writer

	mu       sync.Mutex
	cond     *sync.Cond
	inFlight map[string]bool
	active   int
	queue    chan bgHQJob
	stopOnce sync.Once
	stopCtx  context.Context
	stopFn   context.CancelFunc
	wg       sync.WaitGroup

	CacheHits   atomic.Int64
	InstantRuns atomic.Int64
	HQCompleted atomic.Int64
}

func NewTieredEngine(instant, hq WholeTrackFunc, cache *HQCache, log io.Writer) *TieredEngine {
	ctx, cancel := context.WithCancel(context.Background())
	te := &TieredEngine{
		Instant:  instant,
		HQ:       hq,
		Cache:    cache,
		Log:      log,
		inFlight: map[string]bool{},
		queue:    make(chan bgHQJob, 4),
		stopCtx:  ctx,
		stopFn:   cancel,
	}
	te.cond = sync.NewCond(&te.mu)
	if hq != nil && cache != nil {
		te.wg.Add(1)
		go te.bgLoop()
	}
	return te
}

func (te *TieredEngine) bgLoop() {
	defer te.wg.Done()
	for {
		select {
		case <-te.stopCtx.Done():
			return
		case job := <-te.queue:
			stems, err := te.HQ(te.stopCtx, job.mix, job.rate)
			if err == nil {
				err = te.Cache.Put(job.digest, stems, job.rate)
				if err == nil {
					te.HQCompleted.Add(1)
				}
			}
			if err != nil && te.Log != nil && te.stopCtx.Err() == nil {
				fmt.Fprintf(te.Log, "enginex-stemsd: background hq render (%s): %v\n", job.digest[:12], err)
			}
			te.mu.Lock()
			delete(te.inFlight, job.digest)
			te.active--
			te.cond.Broadcast()
			te.mu.Unlock()
		}
	}
}

func (te *TieredEngine) enqueueHQ(digest string, mix [][]float32, rate float64) {
	if te.HQ == nil || te.Cache == nil || len(mix) == 0 || len(mix[0]) == 0 {
		return
	}
	te.mu.Lock()
	if te.inFlight[digest] {
		te.mu.Unlock()
		return
	}
	cp := make([][]float32, len(mix))
	for c := range mix {
		cp[c] = slicesCloneF32(mix[c])
	}
	select {
	case te.queue <- bgHQJob{digest: digest, rate: rate, mix: cp}:
		te.inFlight[digest] = true
		te.active++
	default:
		// Bounded backlog full: skip background job rather than growing RAM unboundedly.
	}
	te.mu.Unlock()
}

// WaitIdle blocks until all queued background HQ renders have completed or ctx ends.
func (te *TieredEngine) WaitIdle(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		te.mu.Lock()
		for te.active > 0 {
			te.cond.Wait()
		}
		te.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (te *TieredEngine) Close() {
	te.stopOnce.Do(func() {
		te.stopFn()
		te.wg.Wait()
	})
}

// ServeSession speaks the Engine 5.0.4 child protocol over (in, out):
// if the track is in HQCache, serves HQ stems immediately; otherwise runs Instant
// and schedules HQ in the background.
func (te *TieredEngine) ServeSession(ctx context.Context, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 256<<10)
	w := bufio.NewWriterSize(out, 256<<10)
	writeHeader := func(lines ...string) error {
		for _, l := range lines {
			if _, err := w.WriteString(l + "\n"); err != nil {
				return err
			}
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
		return w.Flush()
	}
	if err := writeHeader("session=FFFFFFFFFFFFFFFF"); err != nil {
		return err
	}
	init, err := readNativeHeader(r, true)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	for _, reqKey := range []string{"numChannels", "sampleRate", "maxBlockSize"} {
		if _, ok := init[reqKey]; !ok {
			return fmt.Errorf("stems tiered: init header lacks %s", reqKey)
		}
	}
	channels, err := strconv.Atoi(init["numChannels"])
	if err != nil || (channels != 1 && channels != 2) {
		return fmt.Errorf("stems tiered: unsupported numChannels %q", init["numChannels"])
	}
	sampleRate, err := strconv.ParseFloat(init["sampleRate"], 64)
	if err != nil || !(sampleRate >= 8000 && sampleRate <= nativeMaxRate) {
		return fmt.Errorf("stems tiered: unsupported sampleRate %q", init["sampleRate"])
	}
	maxBlock, err := strconv.Atoi(init["maxBlockSize"])
	if err != nil || maxBlock < 1 || maxBlock > nativeMaxBlock {
		return fmt.Errorf("stems tiered: unsupported maxBlockSize %q", init["maxBlockSize"])
	}
	if err := writeHeader("preparing=ready"); err != nil {
		return err
	}
	if err := writeHeader("preparing=ready", fmt.Sprintf("numReturnChannels=%d", channels*4)); err != nil {
		return err
	}

	mix := make([][]float32, channels)
	var rawPCM []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fields, err := readNativeHeader(r, false)
		if err != nil {
			return err
		}
		switch fields["action"] {
		case "done":
			var stems [][][]float32
			totalFrames := len(mix[0])
			if totalFrames > 0 {
				digest := TrackAudioDigest(mix, sampleRate)
				if cached, ok := te.Cache.Get(digest, mix, sampleRate); ok {
					te.CacheHits.Add(1)
					stems = cached
				} else {
					te.InstantRuns.Add(1)
					stems, err = te.Instant(ctx, mix, sampleRate)
					if err != nil {
						return err
					}
					te.enqueueHQ(digest, mix, sampleRate)
				}
			} else {
				stems = emptyStems(channels)
			}
			var outBuf []byte
			for pos := 0; pos < totalFrames; {
				n := min(maxBlock, totalFrames-pos)
				if _, err := w.WriteString("returnFrames=" + strconv.Itoa(n) + "\n\n"); err != nil {
					return err
				}
				need := 4 * channels * n * 4
				if cap(outBuf) < need {
					outBuf = make([]byte, need)
				}
				outBuf = outBuf[:need]
				off := 0
				for s := 0; s < 4; s++ {
					for c := 0; c < channels; c++ {
						plane := stems[s][c][pos : pos+n]
						for k := 0; k < n; k++ {
							binary.LittleEndian.PutUint32(outBuf[off:], math.Float32bits(plane[k]))
							off += 4
						}
					}
				}
				if _, err := w.Write(outBuf); err != nil {
					return err
				}
				if err := w.Flush(); err != nil {
					return err
				}
				pos += n
			}
			_, err := w.WriteString("returnFrames=0\n\n")
			if err != nil {
				return err
			}
			return w.Flush()

		case "block":
			ns, err := strconv.Atoi(fields["numSamples"])
			if err != nil || ns < 0 || ns > maxBlock {
				return fmt.Errorf("stems tiered: invalid numSamples %q", fields["numSamples"])
			}
			if ns > 0 {
				need := ns * channels * 4
				if cap(rawPCM) < need {
					rawPCM = make([]byte, need)
				}
				rawPCM = rawPCM[:need]
				if _, err := io.ReadFull(r, rawPCM); err != nil {
					return errors.New("stems tiered: truncated PCM payload")
				}
				for c := 0; c < channels; c++ {
					base := c * ns * 4
					for k := 0; k < ns; k++ {
						v := math.Float32frombits(binary.LittleEndian.Uint32(rawPCM[base+k*4:]))
						if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
							return errors.New("stems tiered: non-finite PCM")
						}
						mix[c] = append(mix[c], v)
					}
				}
			}
			// Acknowledge block immediately so both pipelined and lockstep clients advance without waiting.
			if _, err := w.WriteString("returnFrames=0\n\n"); err != nil {
				return err
			}
			if r.Buffered() == 0 {
				if err := w.Flush(); err != nil {
					return err
				}
			}

		default:
			return fmt.Errorf("stems tiered: unsupported action %q", fields["action"])
		}
	}
}

// WorkerWholeTrackFunc adapts an external Engine-5.0.4 child command (such as
// python3 worker.py --backend htdemucs) into a WholeTrackFunc for background HQ caching.
func WorkerWholeTrackFunc(worker []string) WholeTrackFunc {
	return func(ctx context.Context, x [][]float32, sampleRate float64) ([][][]float32, error) {
		if len(worker) == 0 {
			return nil, errors.New("empty worker command")
		}
		ch := len(x)
		total := len(x[0])
		cmd := exec.CommandContext(ctx, worker[0], worker[1:]...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			stdin.Close()
			return nil, err
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			stdin.Close()
			return nil, err
		}
		writeErrCh := make(chan error, 1)
		go func() {
			bw := bufio.NewWriterSize(stdin, 64<<10)
			fmt.Fprintf(bw, "numChannels=%d\nsampleRate=%.0f\nmaxBlockSize=4096\n\n", ch, sampleRate)
			buf := make([]byte, 4096*ch*4)
			for pos := 0; pos < total; pos += 4096 {
				n := min(4096, total-pos)
				fmt.Fprintf(bw, "action=block\nnumSamples=%d\n\n", n)
				off := 0
				for c := 0; c < ch; c++ {
					for k := 0; k < n; k++ {
						binary.LittleEndian.PutUint32(buf[off:], math.Float32bits(x[c][pos+k]))
						off += 4
					}
				}
				if _, err := bw.Write(buf[:off]); err != nil {
					stdin.Close()
					writeErrCh <- err
					return
				}
			}
			bw.WriteString("action=done\n\n")
			err := bw.Flush()
			stdin.Close()
			writeErrCh <- err
		}()

		br := bufio.NewReaderSize(stdout, 64<<10)
		out := emptyStems(ch)
		gotZeroAfterDone := false
		for {
			f, err := readNativeHeader(br, true)
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = cmd.Wait()
				return nil, err
			}
			rfStr, ok := f["returnFrames"]
			if !ok {
				continue
			}
			rf, err := strconv.Atoi(rfStr)
			if err != nil || rf < 0 || rf > 65536 {
				_ = cmd.Wait()
				return nil, fmt.Errorf("worker reply bad returnFrames %q", rfStr)
			}
			if rf == 0 {
				if len(out[0][0]) == total {
					gotZeroAfterDone = true
					break
				}
				continue
			}
			raw := make([]byte, rf*4*ch*4)
			if _, err := io.ReadFull(br, raw); err != nil {
				_ = cmd.Wait()
				return nil, err
			}
			off := 0
			for s := 0; s < 4; s++ {
				for c := 0; c < ch; c++ {
					for k := 0; k < rf; k++ {
						out[s][c] = append(out[s][c], math.Float32frombits(binary.LittleEndian.Uint32(raw[off:])))
						off += 4
					}
				}
			}
		}
		werr := <-writeErrCh
		cerr := cmd.Wait()
		if cerr != nil {
			return nil, fmt.Errorf("worker failed: %w (%s)", cerr, bytes.TrimSpace(stderr.Bytes()))
		}
		if werr != nil {
			return nil, werr
		}
		if !gotZeroAfterDone || len(out[0][0]) != total {
			return nil, fmt.Errorf("worker incomplete output (%d of %d frames)", len(out[0][0]), total)
		}
		return out, nil
	}
}

// DownloadVerifiedAsset downloads url to dstPath atomically after verifying expectedSHA256.
func DownloadVerifiedAsset(ctx context.Context, url, dstPath, expectedSHA256 string) error {
	if raw, err := os.ReadFile(dstPath); err == nil {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) == expectedSHA256 {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", dstPath, os.Getpid())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 512<<20))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		return cmpErr(copyErr, closeErr)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedSHA256 {
		os.Remove(tmp)
		return fmt.Errorf("download %s: SHA-256 mismatch (got %s, want %s)", filepath.Base(dstPath), got, expectedSHA256)
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
