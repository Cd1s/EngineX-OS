package webui

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
	"github.com/Cd1s/EngineX-OS/pkg/prolink"
	"github.com/Cd1s/EngineX-OS/pkg/rekordbox"
	"github.com/Cd1s/EngineX-OS/third_party/stagelinq-eaas/enginelibrary"
	"github.com/Cd1s/EngineX-OS/third_party/stagelinq-eaas/networktrust"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The datagram an SC5000's Engine 5.3.0 accepted as a Remote Library (twin sandbox,
// "Found EaaS source {5e305b0e-...} "rekordbox probe" "4.3.0" 1 "_"").
const eaasAccepted = "4541415301015e305b0e7a1b4c2d9f10523055e0aa010000001e00720065006b006f007200640062006f0078002000700072006f0062006500000016677270633a2f2f3132372e302e302e313a35303535310000000a0034002e0033002e0030010000000200" + "5f"

func TestEaasAnswerIsTheDatagramEngineAccepted(t *testing.T) {
	id := uuid.MustParse("5e305b0e-7a1b-4c2d-9f10-523055e0aa01")
	got := hex.EncodeToString(eaasAnswer(id, "rekordbox probe", "grpc://127.0.0.1:50551", "4.3.0"))
	if got != eaasAccepted {
		t.Fatalf("answer\n got %s\nwant %s", got, eaasAccepted)
	}
}

// eaasLink is the computer's library with track facts and analysis over dbserver.
type eaasLink struct{ fakeRbxLink }

func (eaasLink) Metadata(id uint32) ([]prolink.RekordboxItem, error) {
	return []prolink.RekordboxItem{{Kind: 4, Label: "First"}, {Kind: 7, Label: "Artist One"}, {Kind: 0xb, ID: 3}, {Kind: 0xd, ID: 12000}}, nil
}

func (eaasLink) Analysis(id uint32, tag, file string) ([]byte, error) {
	switch tag + "." + file {
	case "PQTZ.DAT": // beats at 0.5 s and 1 s, 120 BPM
		b := make([]byte, 24+16)
		copy(b, "PQTZ")
		binary.BigEndian.PutUint32(b[4:], 24)
		binary.BigEndian.PutUint32(b[8:], 40)
		binary.BigEndian.PutUint32(b[20:], 2)
		for i, ms := range []uint32{500, 1000} {
			p := b[24+8*i:]
			binary.BigEndian.PutUint16(p, uint16(i+1))
			binary.BigEndian.PutUint16(p[2:], 12000)
			binary.BigEndian.PutUint32(p[4:], ms)
		}
		return b, nil
	case "PCOB.DAT": // hot cue A at 2 s
		b := make([]byte, 24+56)
		copy(b, "PCOB")
		binary.BigEndian.PutUint32(b[4:], 24)
		binary.BigEndian.PutUint32(b[8:], uint32(len(b)))
		binary.BigEndian.PutUint32(b[12:], 1)
		binary.BigEndian.PutUint16(b[18:], 1)
		p := b[24:]
		copy(p, "PCPT")
		binary.BigEndian.PutUint32(p[4:], 28)
		binary.BigEndian.PutUint32(p[8:], 56)
		binary.BigEndian.PutUint32(p[12:], 1)
		p[28] = 1
		binary.BigEndian.PutUint32(p[32:], 2000)
		binary.BigEndian.PutUint32(p[36:], 0xffffffff)
		return b, nil
	}
	return nil, nil
}

// flac48k is a FLAC file at 48 kHz, 3 s, padded to size: only its header is real.
func flac48k(size int) []byte {
	b := make([]byte, size)
	copy(b, "fLaC")
	b[4], b[7] = 0x80, 34 // last block, STREAMINFO
	si := uint64(48000)<<44 | uint64(1)<<41 | uint64(15)<<36 | 3*48000
	binary.BigEndian.PutUint32(b[8:], 4096<<16|4096)
	binary.BigEndian.PutUint64(b[8+10:], si)
	return b
}

func TestEaasServesTheRekordboxLibraryWithItsAnalysis(t *testing.T) {
	paths := map[uint32]string{1: "/Users/dj/Music/x.flac", 2: "/Users/dj/Music/y.flac"}
	e := newRbxEnv(t, paths)
	audio := flac48k(4096)
	os.WriteFile(filepath.Join(e.share, "x.flac"), audio, 0o644)
	dialRekordbox = func(context.Context, net.IP, byte) (rbxLink, error) { return eaasLink{fakeRbxLink{paths}}, nil }
	free, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	port := free.LocalAddr().(*net.UDPAddr).Port
	free.Close()
	oldPort, oldG, oldH, oldHere := eaasDiscoveryPort, eaasGRPCAddr, eaasHTTPAddr, eaasFromHere
	t.Cleanup(func() { eaasDiscoveryPort, eaasGRPCAddr, eaasHTTPAddr, eaasFromHere = oldPort, oldG, oldH, oldHere })
	eaasDiscoveryPort, eaasGRPCAddr, eaasHTTPAddr = port, "127.0.0.1:0", "127.0.0.1:0"
	eaasFromHere = func(ip net.IP) bool { return ip.IsLoopback() }
	srv, err := startEaas(e.s, "127.0.0.1", "dj-macbook")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.stop)

	// Discovery: one answer per service, both from 127.0.0.1, read as Engine reads them.
	// Like Engine's, an unconnected socket: the answers come from another port of 127.0.0.1.
	c, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer c.Close()
	c.WriteToUDP(eaasRequest, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	urls := map[string]string{}
	for range 2 {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 512)
		n, err := c.Read(b)
		if err != nil {
			t.Fatalf("discovery answer: %v", err)
		}
		u := eaasReadURL(t, b[:n])
		urls[strings.SplitN(u, ":", 2)[0]] = strings.SplitN(u, "://", 2)[1]
	}
	if urls["grpc"] == "" || urls["http"] == "" {
		t.Fatalf("services %v", urls)
	}

	conn, err := grpc.NewClient(urls["grpc"], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := context.Background()
	trust, err := networktrust.NewNetworkTrustServiceClient(conn).CreateTrust(ctx, &networktrust.CreateTrustRequest{})
	if err != nil || trust.GetGranted() == nil {
		t.Fatalf("trust %v %v", trust, err)
	}
	lib := enginelibrary.NewEngineLibraryServiceClient(conn)
	libs, err := lib.GetLibraries(ctx, &enginelibrary.GetLibrariesRequest{})
	if err != nil || len(libs.Libraries) != 1 || libs.Libraries[0].GetLogo() != enginelibrary.LibraryLogo_LIBRARY_LOGO_REKORDBOX || libs.Libraries[0].GetTitle() != "rekordbox (dj-macbook)" {
		t.Fatalf("libraries %v %v", libs, err)
	}
	tree, err := lib.GetLibrary(ctx, &enginelibrary.GetLibraryRequest{})
	if err != nil || len(tree.Playlists) != 2 || tree.Playlists[0].GetId() != "all" || tree.Playlists[1].GetTitle() != "Sets" || tree.Playlists[1].Playlists[0].GetId() != "9" {
		t.Fatalf("playlists %v %v", tree, err)
	}
	st, err := lib.GetTracks(ctx, &enginelibrary.GetTracksRequest{PlaylistId: ptr("all")})
	if err != nil {
		t.Fatal(err)
	}
	page, err := st.Recv()
	if err != nil || len(page.Tracks) != 2 || page.Tracks[0].Metadata.GetTitle() != "First" || page.Tracks[0].Metadata.GetBpm() != 128 {
		t.Fatalf("tracks %v %v", page, err)
	}

	got, err := lib.GetTrack(ctx, &enginelibrary.GetTrackRequest{TrackId: ptr("1")})
	if err != nil {
		t.Fatal(err)
	}
	if u := got.Blob.GetUrl(); u.GetUrl() != "<1.flac>" || u.GetFileSize() != uint32(len(audio)) {
		t.Fatalf("blob %v", u)
	}
	// rekordbox's grid at the file's own 48 kHz, as rb-bridge would write it into m.db.
	want, _ := enginedb.EncodePerformance(rekordbox.Track{ID: 1, SampleRate: 48000, SampleCount: 3 * 48000, Duration: 3, BPM: 120, Title: "First"}, (eaasLink{}).analysis(t))
	pd := got.PerformanceData
	if !bytes.Equal(pd.BeatGrid, want.Beats) || len(want.Beats) == 0 {
		t.Fatalf("beat grid differs from EncodePerformance at 48 kHz")
	}
	if len(pd.QuickCues) != 1 || pd.QuickCues[0].GetKey() != 0 || pd.QuickCues[0].Value.GetPosition() != 96000 || pd.GetInitialImportSource() != enginelibrary.InitialImportSource_INITIAL_IMPORT_SOURCE_REKORDBOX || pd.GetBpm() != 120 {
		t.Fatalf("cue A or facts: %v", pd)
	}

	req, _ := http.NewRequest("GET", "http://"+urls["http"]+"/download/%3C1.flac%3E", nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "fLaC" {
		t.Fatalf("download %d %q", resp.StatusCode, body)
	}
}

func (l eaasLink) analysis(t *testing.T) *rekordbox.Analysis { return eaasAnalysis(l, 1) }

// eaasReadURL reads a discovery answer as Engine's client does and returns its service URL.
func eaasReadURL(t *testing.T, b []byte) string {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("EAAS\x01\x01")) || len(b) < 26 {
		t.Fatalf("answer %x", b)
	}
	r := bytes.NewReader(b[22:])
	skip := func() {
		var n uint32
		binary.Read(r, binary.BigEndian, &n)
		r.Seek(int64(n), io.SeekCurrent)
	}
	skip() // name
	var n uint32
	binary.Read(r, binary.BigEndian, &n)
	u := make([]byte, n)
	r.Read(u)
	skip() // version
	typ, _ := r.ReadByte()
	skip()
	if typ != 1 || r.Len() != 0 {
		t.Fatalf("answer type %d, %d bytes left", typ, r.Len())
	}
	return string(u)
}
