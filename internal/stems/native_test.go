package stems

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSGEMMSIMDMatchesScalar(t *testing.T) {
	rng := rand.New(rand.NewPCG(197, 5305))
	for _, m := range []int{1, 3, 4, 7, 19, 32} {
		for _, n := range []int{1, 3, 4, 8, 13, 33} {
			for _, k := range []int{1, 5, 64, 131} {
				a := make([]float32, m*k)
				b := make([]float32, k*n)
				for i := range a {
					a[i] = float32(rng.NormFloat64() * 0.25)
				}
				for i := range b {
					b[i] = float32(rng.NormFloat64() * 0.25)
				}
				want := make([]float32, m*n)
				for i := range want {
					want[i] = float32(i) * 0.01
				}
				got1 := slicesCloneF32(want)
				got4 := slicesCloneF32(want)

				alpha := float32(0.75)
				for r := 0; r < m; r++ {
					for p := 0; p < k; p++ {
						av := a[r*k+p] * alpha
						for c := 0; c < n; c++ {
							want[r*n+c] += av * b[p*n+c]
						}
					}
				}
				sgemmAdd(m, n, k, alpha, a, b, got1, 1)
				sgemmAdd(m, n, k, alpha, a, b, got4, 4)

				for idx := range want {
					if d := math.Abs(float64(got1[idx] - want[idx])); d > 1e-4 {
						t.Fatalf("m=%d n=%d k=%d w=1 idx=%d got=%g want=%g diff=%g", m, n, k, idx, got1[idx], want[idx], d)
					}
					if d := math.Abs(float64(got4[idx] - want[idx])); d > 1e-4 {
						t.Fatalf("m=%d n=%d k=%d w=4 idx=%d got=%g want=%g diff=%g", m, n, k, idx, got4[idx], want[idx], d)
					}
				}
			}
		}
	}
}

func TestDSPRoundTripsAndLoaders(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 99))
	for _, n := range []int{2048, 4096} {
		fft := newRadix2FFT(n)
		x := make([]float32, n)
		for i := range x {
			x[i] = float32(rng.NormFloat64() * 0.3)
		}
		re := make([]float32, n/2+1)
		im := make([]float32, n/2+1)
		rec := make([]float32, n)
		scratch := make([]float64, n)
		fft.rfft(x, re, im, scratch)
		fft.irfft(re, im, rec, scratch)
		for i := range x {
			if d := math.Abs(float64(rec[i] - x[i])); d > 1e-5 {
				t.Fatalf("fft n=%d i=%d diff=%g", n, i, d)
			}
		}
	}

	// HTDemucs _spec / _ispec exact reconstruction in the interior [1536 : length-1536]
	// (border frames 0..1 end at 2048+1536+1536=5120; inside that margin _ispec(_spec(x)) == x).
	htd := NewTestHTDemucsModel(16384)
	for _, length := range []int{8192, 12345} {
		sig := make([]float32, length)
		for i := range sig {
			sig[i] = float32(0.4*math.Sin(2*math.Pi*220*float64(i)/44100) + 0.2*math.Cos(2*math.Pi*1760*float64(i)/44100))
		}
		re, im, le := htd.htdSpec(sig)
		rec := htd.htdISpec(re, im, le, length)
		for i := 1536; i < length-1536; i++ {
			if d := math.Abs(float64(rec[i] - sig[i])); d > 2e-5 {
				t.Fatalf("htdSpec/ISpec len=%d i=%d got=%g want=%g diff=%g", length, i, rec[i], sig[i], d)
			}
		}
	}

	// float16 edge cases + safetensors F16/F32 loader
	if v := float16ToFloat32(0x3c00); v != 1.0 {
		t.Fatalf("f16 1.0 = %g", v)
	}
	if v := float16ToFloat32(0xc000); v != -2.0 {
		t.Fatalf("f16 -2.0 = %g", v)
	}
	if v := float16ToFloat32(0x0001); !(v > 0 && v < 1e-6) {
		t.Fatalf("f16 subnormal = %g", v)
	}
	hdrObj := map[string]any{
		"a": map[string]any{"dtype": "F16", "shape": []int{2}, "data_offsets": []int{0, 4}},
		"b": map[string]any{"dtype": "F32", "shape": []int{1}, "data_offsets": []int{4, 8}},
	}
	hdrJSON, _ := json.Marshal(hdrObj)
	var stBuf bytes.Buffer
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(hdrJSON)))
	stBuf.Write(lenBuf[:])
	stBuf.Write(hdrJSON)
	var dataBuf [8]byte
	binary.LittleEndian.PutUint16(dataBuf[0:2], 0x3c00) // 1.0
	binary.LittleEndian.PutUint16(dataBuf[2:4], 0xc000) // -2.0
	binary.LittleEndian.PutUint32(dataBuf[4:8], math.Float32bits(3.5))
	stBuf.Write(dataBuf[:])
	parsed, err := parseSafetensors(stBuf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if parsed["a"].Data[0] != 1.0 || parsed["a"].Data[1] != -2.0 || parsed["b"].Data[0] != 3.5 {
		t.Fatalf("unexpected safetensors values: %+v", parsed)
	}

	// Verified asset download: accepts valid SHA-256, rejects tampered payload
	payload := []byte("verified model weights payload")
	sum := sha256.Sum256(payload)
	wantHex := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	badDst := filepath.Join(dir, "bad.bin")
	if err := DownloadVerifiedAsset(context.Background(), srv.URL, badDst, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected SHA-256 mismatch error")
	}
	if _, err := os.Stat(badDst); !os.IsNotExist(err) {
		t.Fatal("corrupt download must not leave destination file")
	}
	goodDst := filepath.Join(dir, "good.bin")
	if err := DownloadVerifiedAsset(context.Background(), srv.URL, goodDst, wantHex); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(goodDst)
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("verified download content mismatch: %v", err)
	}
}

func buildEngineWireRequest(channels int, rate float64, maxBlock int, x [][]float32, leadingBlank bool) []byte {
	var b bytes.Buffer
	if leadingBlank {
		b.WriteString("\n\r\n")
	}
	fmt.Fprintf(&b, "numChannels=%d\nsampleRate=%.0f\nmaxBlockSize=%d\n\n", channels, rate, maxBlock)
	total := 0
	if channels > 0 {
		total = len(x[0])
	}
	for pos := 0; pos < total; pos += maxBlock {
		n := min(maxBlock, total-pos)
		fmt.Fprintf(&b, "action=block\nnumSamples=%d\n\n", n)
		raw := make([]byte, n*channels*4)
		off := 0
		for c := 0; c < channels; c++ {
			for k := 0; k < n; k++ {
				binary.LittleEndian.PutUint32(raw[off:], math.Float32bits(x[c][pos+k]))
				off += 4
			}
		}
		b.Write(raw)
	}
	b.WriteString("action=done\n\n")
	return b.Bytes()
}

func parseEngineWireReply(t *testing.T, raw []byte, channels, maxBlock int) [][][]float32 {
	t.Helper()
	sess := &Session{}
	_ = sess // also validate via stream
	r := bytes.NewReader(raw)
	br := bufio.NewReader(r)
	out := emptyStems(channels)
	var replies []int
	for {
		f, err := readNativeHeader(br, false)
		if err != nil {
			break
		}
		v, ok := f["returnFrames"]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxBlock {
			t.Fatalf("bad returnFrames %q (max %d)", v, maxBlock)
		}
		replies = append(replies, n)
		if n > 0 {
			pcm := make([]byte, n*4*channels*4)
			if _, err := io.ReadFull(br, pcm); err != nil {
				t.Fatalf("short PCM read: %v", err)
			}
			off := 0
			for s := 0; s < 4; s++ {
				for c := 0; c < channels; c++ {
					for k := 0; k < n; k++ {
						out[s][c] = append(out[s][c], math.Float32frombits(binary.LittleEndian.Uint32(pcm[off:])))
						off += 4
					}
				}
			}
		}
	}
	if len(replies) == 0 || replies[len(replies)-1] != 0 {
		t.Fatalf("missing closing returnFrames=0: %v", replies)
	}
	return out
}

func TestNativeHPSSParityWithWorkerPy(t *testing.T) {
	rng := rand.New(rand.NewPCG(6596, 1))
	for _, ch := range []int{1, 2} {
		for _, length := range []int{0, 1, 511, 512, 1024, 1025, 12347} {
			x := make([][]float32, ch)
			for c := 0; c < ch; c++ {
				x[c] = make([]float32, length)
				for i := 0; i < length; i++ {
					x[c][i] = float32(rng.NormFloat64() * 0.1)
				}
			}
			sep, err := NewHPSSSeparator(ch, 44100)
			if err != nil {
				t.Fatal(err)
			}
			got := emptyStems(ch)
			for i := 0; i < length; i += 237 {
				end := min(i+237, length)
				piece := make([][]float32, ch)
				for c := 0; c < ch; c++ {
					piece[c] = x[c][i:end]
				}
				y, err := sep.Feed(context.Background(), piece)
				if err != nil {
					t.Fatal(err)
				}
				for s := 0; s < 4; s++ {
					for c := 0; c < ch; c++ {
						got[s][c] = append(got[s][c], y[s][c]...)
					}
				}
			}
			yd, err := sep.Drain(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for s := 0; s < 4; s++ {
				for c := 0; c < ch; c++ {
					got[s][c] = append(got[s][c], yd[s][c]...)
					if len(got[s][c]) != length {
						t.Fatalf("ch=%d len=%d got=%d", ch, length, len(got[s][c]))
					}
				}
			}
			for c := 0; c < ch; c++ {
				for i := 0; i < length; i++ {
					sum := got[0][c][i] + got[1][c][i] + got[2][c][i] + got[3][c][i]
					if d := math.Abs(float64(sum - x[c][i])); d > 5e-6 {
						t.Fatalf("ch=%d len=%d i=%d sum diff=%g", ch, length, i, d)
					}
				}
			}
		}
	}

	// Non-hop-aligned block limits + stray blank lines before init
	for _, limit := range []int{1, 511, 513, 1000} {
		x := make([][]float32, 2)
		for c := 0; c < 2; c++ {
			x[c] = make([]float32, 2107)
			for i := range x[c] {
				x[c][i] = float32(rng.NormFloat64() * 0.05)
			}
		}
		req := buildEngineWireRequest(2, 44100, limit, x, limit == 511)
		var out bytes.Buffer
		err := ServeNative(context.Background(), bytes.NewReader(req), &out, func(ch int, rate float64) (StreamSeparator, error) {
			return NewHPSSSeparator(ch, rate)
		})
		if err != nil {
			t.Fatalf("limit=%d: %v", limit, err)
		}
		stems := parseEngineWireReply(t, out.Bytes(), 2, limit)
		for c := 0; c < 2; c++ {
			if len(stems[0][c]) != 2107 {
				t.Fatalf("limit=%d got %d frames", limit, len(stems[0][c]))
			}
			for i := 0; i < 2107; i++ {
				sum := stems[0][c][i] + stems[1][c][i] + stems[2][c][i] + stems[3][c][i]
				if d := math.Abs(float64(sum - x[c][i])); d > 5e-6 {
					t.Fatalf("limit=%d i=%d diff=%g", limit, i, d)
				}
			}
		}
	}

	// Reject invalid/unbounded init headers
	for _, bad := range []string{
		"numChannels=200\nsampleRate=44100\nmaxBlockSize=1024\n\n",
		"numChannels=2\nsampleRate=44100\nmaxBlockSize=1000000000\n\n",
		"numChannels=2\n\n",
		"x=1\nx=2\n\n",
	} {
		err := ServeNative(context.Background(), bytes.NewReader([]byte(bad)), &bytes.Buffer{}, func(ch int, rate float64) (StreamSeparator, error) {
			return NewHPSSSeparator(ch, rate)
		})
		if err == nil {
			t.Fatalf("expected error for invalid header %q", bad)
		}
	}
}

func TestSpleeterAndHTDemucsForwardAndSum(t *testing.T) {
	rng := rand.New(rand.NewPCG(197, 2026))
	sp := NewTestSpleeterModel()
	htd := NewTestHTDemucsModel(8192)

	cases := []struct {
		rate   float64
		ch     int
		length int
	}{
		{44100, 2, 0},
		{44100, 2, 2048},
		{48000, 1, 1023},
	}
	for _, tc := range cases {
		rate, ch, length := tc.rate, tc.ch, tc.length
		x := make([][]float32, ch)
		for c := 0; c < ch; c++ {
			x[c] = make([]float32, length)
			for i := 0; i < length; i++ {
				x[c][i] = float32(rng.NormFloat64() * 0.1)
			}
		}
		for name, fn := range map[string]WholeTrackFunc{"spleeter": sp.Separate, "htdemucs": htd.Separate} {
			y, err := fn(context.Background(), x, rate)
			if err != nil {
				t.Fatalf("%s rate=%.0f ch=%d len=%d: %v", name, rate, ch, length, err)
			}
			for c := 0; c < ch; c++ {
				if len(y[0][c]) != length {
					t.Fatalf("%s len=%d got=%d", name, length, len(y[0][c]))
				}
				for i := 0; i < length; i++ {
					sum := y[0][c][i] + y[1][c][i] + y[2][c][i] + y[3][c][i]
					if math.IsNaN(float64(sum)) || math.IsInf(float64(sum), 0) {
						t.Fatalf("%s non-finite output", name)
					}
					if d := math.Abs(float64(sum - x[c][i])); d > 2e-6 {
						t.Fatalf("%s rate=%.0f ch=%d i=%d sum diff=%g", name, rate, ch, i, d)
					}
				}
			}
		}
	}

	// ChunkedSeparator alignment check (matching test_demucs_chunks_stay_aligned)
	fakeFixed := func(_ context.Context, x [][]float32, _ float64) ([][][]float32, error) {
		out := emptyStems(len(x))
		for c := range x {
			n := len(x[c])
			for s := 0; s < 4; s++ {
				out[s][c] = make([]float32, n)
			}
			for i := 0; i < n; i++ {
				out[0][c][i] = x[c][i] * 0.5
				out[1][c][i] = x[c][i] * 0.25
				out[2][c][i] = 0
				out[3][c][i] = x[c][i] - out[0][c][i] - out[1][c][i]
			}
		}
		return out, nil
	}
	for _, ch := range []int{1, 2} {
		for _, length := range []int{0, 1, 399, 400, 401, 3001} {
			x := make([][]float32, ch)
			for c := 0; c < ch; c++ {
				x[c] = make([]float32, length)
				for i := range x[c] {
					x[c][i] = float32(rng.NormFloat64() * 0.1)
				}
			}
			cs, err := NewChunkedSeparator(ch, 8000, 0.05, 0.01, fakeFixed)
			if err != nil {
				t.Fatal(err)
			}
			got := emptyStems(ch)
			for i := 0; i < length; i += 237 {
				end := min(i+237, length)
				piece := make([][]float32, ch)
				for c := 0; c < ch; c++ {
					piece[c] = x[c][i:end]
				}
				y, err := cs.Feed(context.Background(), piece)
				if err != nil {
					t.Fatal(err)
				}
				for s := 0; s < 4; s++ {
					for c := 0; c < ch; c++ {
						got[s][c] = append(got[s][c], y[s][c]...)
					}
				}
			}
			yd, err := cs.Drain(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for s := 0; s < 4; s++ {
				for c := 0; c < ch; c++ {
					got[s][c] = append(got[s][c], yd[s][c]...)
				}
			}
			for c := 0; c < ch; c++ {
				for i := 0; i < length; i++ {
					if d := math.Abs(float64(got[0][c][i] - x[c][i]*0.5)); d > 2e-6 {
						t.Fatalf("chunk align ch=%d len=%d i=%d diff=%g", ch, length, i, d)
					}
				}
			}
		}
	}
}

func TestTieredInstantThenCachedHQOverWire(t *testing.T) {
	cacheDir := t.TempDir()
	cache, err := NewHQCache(cacheDir, 1) // 1 MiB cap to also test eviction
	if err != nil {
		t.Fatal(err)
	}
	scaleSplit := func(vocalShare float32) WholeTrackFunc {
		return func(_ context.Context, x [][]float32, _ float64) ([][][]float32, error) {
			out := emptyStems(len(x))
			for c := range x {
				n := len(x[c])
				for s := 0; s < 4; s++ {
					out[s][c] = make([]float32, n)
				}
				for i, v := range x[c] {
					out[0][c][i] = v * vocalShare
					out[1][c][i] = v * 0.125
					out[2][c][i] = v * 0.125
					out[3][c][i] = v - out[0][c][i] - out[1][c][i] - out[2][c][i]
				}
			}
			return out, nil
		}
	}

	te := NewTieredEngine(scaleSplit(0.25), scaleSplit(0.75), cache, nil)
	defer te.Close()

	testBearer := strings.Repeat("t", 24)
	backend := &Backend{
		Secret: testBearer,
		Render: te.ServeSession,
		Slot:   make(chan struct{}, 2),
		Name:   "spleeter+htdemucs",
	}
	srv := httptest.NewServer(backend)
	defer srv.Close()

	x := make([][]float32, 2)
	for c := 0; c < 2; c++ {
		x[c] = make([]float32, 4096)
		for i := range x[c] {
			x[c][i] = float32(0.5 * math.Sin(2*math.Pi*float64(i+100*c)/128.0))
		}
	}
	reqBytes := buildEngineWireRequest(2, 44100, 1024, x, false)

	// 1st render over full stems.Proxy (Framing: 2 int16 packed!) -> instant pass (0.25)
	var out1 bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Proxy(ctx, srv.URL, testBearer, bytes.NewReader(reqBytes), &out1); err != nil {
		t.Fatalf("first Proxy session failed: %v", err)
	}
	stems1 := parseEngineWireReply(t, out1.Bytes(), 2, 1024)
	if te.InstantRuns.Load() != 1 || te.CacheHits.Load() != 0 {
		t.Fatalf("unexpected counters after 1st run: instant=%d hits=%d", te.InstantRuns.Load(), te.CacheHits.Load())
	}
	if d := math.Abs(float64(stems1[0][0][10] - x[0][10]*0.25)); d > 2e-4 {
		t.Fatalf("expected instant 0.25x vocal stem, got %g vs %g", stems1[0][0][10], x[0][10]*0.25)
	}

	// Wait for background HQ pass to populate HQCache
	if err := te.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if te.HQCompleted.Load() != 1 {
		t.Fatalf("expected HQCompleted=1, got %d", te.HQCompleted.Load())
	}

	// 2nd render over the identical wire protocol -> HQ cache hit (0.75), zero new instant runs!
	var out2 bytes.Buffer
	if err := Proxy(ctx, srv.URL, testBearer, bytes.NewReader(reqBytes), &out2); err != nil {
		t.Fatalf("second Proxy session failed: %v", err)
	}
	stems2 := parseEngineWireReply(t, out2.Bytes(), 2, 1024)
	if te.InstantRuns.Load() != 1 || te.CacheHits.Load() != 1 {
		t.Fatalf("unexpected counters after 2nd run: instant=%d hits=%d", te.InstantRuns.Load(), te.CacheHits.Load())
	}
	if d := math.Abs(float64(stems2[0][0][10] - x[0][10]*0.75)); d > 2e-4 {
		t.Fatalf("expected cached HQ 0.75x vocal stem, got %g vs %g", stems2[0][0][10], x[0][10]*0.75)
	}
	for c := 0; c < 2; c++ {
		for i := 0; i < 4096; i++ {
			sum := stems2[0][c][i] + stems2[1][c][i] + stems2[2][c][i] + stems2[3][c][i]
			if d := math.Abs(float64(sum - x[c][i])); d > 1e-4 {
				t.Fatalf("cached HQ sum-to-input violated at c=%d i=%d: diff=%g", c, i, d)
			}
		}
	}

	// Verify LRU cache eviction stays under maxBytes (1 MiB)
	big := make([][][]float32, 4)
	for s := 0; s < 4; s++ {
		big[s] = [][]float32{make([]float32, 40000), make([]float32, 40000)} // 3*2*40000*2 = 480 KB per entry
	}
	for k := 0; k < 4; k++ {
		if err := cache.Put(fmt.Sprintf("evict-%d", k), big, 44100); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	entries, _ := os.ReadDir(cacheDir)
	var totalBytes int64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			totalBytes += fi.Size()
		}
	}
	if totalBytes > 1<<20 {
		t.Fatalf("HQCache exceeded 1 MiB cap: %d bytes", totalBytes)
	}
}

func TestRealWeightsSmoke(t *testing.T) {
	modelsDir := os.Getenv("ENGINEX_TEST_MODELS_DIR")
	if modelsDir == "" {
		t.Skip("set ENGINEX_TEST_MODELS_DIR to run real-weight Spleeter + HTDemucs smoke test")
	}
	sp, err := LoadSpleeterPath(modelsDir)
	if err != nil {
		t.Fatalf("LoadSpleeterPath: %v", err)
	}
	htd, err := LoadHTDemucsSafetensors(filepath.Join(modelsDir, "htdemucs.safetensors"))
	if err != nil {
		t.Fatalf("LoadHTDemucsSafetensors: %v", err)
	}

	// Synthesize 2.5 seconds of stereo 44.1 kHz audio: 65 Hz bass + 440/880 Hz vocal + periodic transient clicks
	const n = 44100 * 5 / 2
	mix := [][]float32{make([]float32, n), make([]float32, n)}
	for i := 0; i < n; i++ {
		tm := float64(i) / 44100.0
		bass := 0.35 * math.Sin(2*math.Pi*65.0*tm)
		vocal := 0.25*math.Sin(2*math.Pi*440.0*tm)*(1+0.2*math.Sin(2*math.Pi*5.5*tm)) + 0.1*math.Sin(2*math.Pi*880.0*tm)
		click := 0.0
		if i%22050 < 200 {
			click = 0.3 * math.Exp(-float64(i%22050)/40.0) * math.Sin(2*math.Pi*3500.0*tm)
		}
		mix[0][i] = float32(bass + vocal + click)
		mix[1][i] = float32(bass + vocal - click*0.5)
	}

	t0 := time.Now()
	spOut, err := sp.Separate(context.Background(), mix, 44100)
	if err != nil {
		t.Fatalf("real Spleeter Separate: %v", err)
	}
	spDur := time.Since(t0)

	t1 := time.Now()
	htdOut, err := htd.Separate(context.Background(), mix, 44100)
	if err != nil {
		t.Fatalf("real HTDemucs Separate: %v", err)
	}
	htdDur := time.Since(t1)

	for name, out := range map[string][][][]float32{"spleeter": spOut, "htdemucs": htdOut} {
		var energy [4]float64
		for c := 0; c < 2; c++ {
			for i := 0; i < n; i++ {
				sum := out[0][c][i] + out[1][c][i] + out[2][c][i] + out[3][c][i]
				if math.IsNaN(float64(sum)) || math.IsInf(float64(sum), 0) {
					t.Fatalf("%s produced non-finite sample", name)
				}
				if d := math.Abs(float64(sum - mix[c][i])); d > 5e-6 {
					t.Fatalf("%s sum mismatch at i=%d: %g", name, i, d)
				}
				for s := 0; s < 4; s++ {
					v := float64(out[s][c][i])
					energy[s] += v * v
				}
			}
		}
		t.Logf("%s real-weights ok: vocals=%.2f bass=%.2f drums=%.2f melody=%.2f", name, energy[0], energy[1], energy[2], energy[3])
		if energy[0]+energy[1]+energy[2] <= 0 {
			t.Fatalf("%s produced silent stems: %v", name, energy)
		}
	}
	t.Logf("real-weight wall time (2.5s stereo): Spleeter=%s, HTDemucs(7.8s seg)=%s", spDur, htdDur)
}
