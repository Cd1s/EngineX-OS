package providers

// Essential subscriptions only get Beatport's player stream: 128k AAC over HLS,
// AES-128 encrypted with a key the API hands the account for playback. Engine has
// no HLS player and the deck has no ffmpeg, so the stream is decrypted here, the
// AAC frames are taken out of MPEG-TS (or packed ADTS), and written as an MP4 file.

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	hlsMaxSegment  = 16 << 20
	hlsParallel    = 8
	maxStreamBytes = 1 << 30
)

type hlsSegment struct {
	url, keyURL string
	iv          []byte
}

// SaveStream writes a track's stream to w as an MP4 file.
func (b *Beatport) SaveStream(ctx context.Context, id string, w io.WriteSeeker) error {
	if !validBeatportID(id) {
		return errors.New("beatport: invalid track ID")
	}
	var r struct {
		URL string `json:"stream_url"`
	}
	if err := b.get(ctx, "/catalog/tracks/"+id+"/stream/", nil, &r); err != nil {
		return err
	}
	text, err := hlsGet(ctx, b.HTTP, r.URL, 1<<20)
	if err != nil {
		return err
	}
	init, segments, err := parseHLS(r.URL, string(text))
	if err != nil {
		return err
	}
	return writeHLS(ctx, b.HTTP, init, segments, w)
}

func hlsGet(ctx context.Context, c *http.Client, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") {
		return nil, errors.New("beatport: invalid stream address")
	}
	req.Header.Set("User-Agent", beatportUserAgent)
	resp, err := client(c).Do(req)
	if err != nil {
		return nil, errors.New("beatport: stream transfer failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("beatport: stream refused: HTTP " + strconv.Itoa(resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("beatport: stream transfer failed")
	}
	return data, nil
}

// hlsAttrs reads an attribute list such as METHOD=AES-128,URI="k",IV=0x01.
func hlsAttrs(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		name, rest, _ := strings.Cut(s, "=")
		var value string
		if strings.HasPrefix(rest, `"`) {
			value, rest, _ = strings.Cut(rest[1:], `"`)
			_, rest, _ = strings.Cut(rest, ",")
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		out[strings.TrimSpace(name)] = value
		s = rest
	}
	return out
}

// parseHLS reads a media playlist. A segment without an IV uses its sequence number.
func parseHLS(playlist, text string) (init string, segments []hlsSegment, err error) {
	base, err := url.Parse(playlist)
	if err != nil {
		return "", nil, errors.New("beatport: invalid playlist address")
	}
	resolve := func(ref string) (string, error) {
		u, err := base.Parse(ref)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return "", errors.New("beatport: invalid playlist")
		}
		return u.String(), nil
	}
	var key string
	var iv []byte
	seq := uint64(0)
	lines := bufio.NewScanner(strings.NewReader(text))
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		tag, rest, _ := strings.Cut(line, ":")
		switch {
		case line == "":
		case tag == "#EXT-X-MEDIA-SEQUENCE":
			seq, _ = strconv.ParseUint(rest, 10, 64)
		case tag == "#EXT-X-STREAM-INF":
			return "", nil, errors.New("beatport: master playlists are not supported")
		case tag == "#EXT-X-KEY":
			a := hlsAttrs(rest)
			key, iv = "", nil
			switch a["METHOD"] {
			case "NONE":
			case "AES-128":
				if key, err = resolve(a["URI"]); err != nil {
					return "", nil, err
				}
				if v := a["IV"]; v != "" {
					if iv, err = hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(v, "0x"), "0X")); err != nil || len(iv) != aes.BlockSize {
						return "", nil, errors.New("beatport: invalid stream IV")
					}
				}
			default:
				return "", nil, errors.New("beatport: unsupported stream encryption")
			}
		case tag == "#EXT-X-MAP":
			if init, err = resolve(hlsAttrs(rest)["URI"]); err != nil {
				return "", nil, err
			}
		case strings.HasPrefix(line, "#"):
		default:
			address, err := resolve(line)
			if err != nil {
				return "", nil, err
			}
			s := hlsSegment{url: address, keyURL: key, iv: iv}
			if key != "" && iv == nil {
				s.iv = make([]byte, aes.BlockSize)
				binary.BigEndian.PutUint64(s.iv[8:], seq)
			}
			segments = append(segments, s)
			seq++
		}
	}
	if len(segments) == 0 {
		return "", nil, errors.New("beatport: empty stream playlist")
	}
	return init, segments, nil
}

func decryptSegment(data, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("beatport: invalid encrypted segment")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, errors.New("beatport: invalid segment padding")
	}
	return data[:len(data)-pad], nil
}

// fetchSegments downloads and decrypts segments a few at a time, handing them to
// use in order.
func fetchSegments(ctx context.Context, c *http.Client, segments []hlsSegment, use func([]byte) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := map[string][]byte{}
	for _, s := range segments {
		if s.keyURL == "" || keys[s.keyURL] != nil {
			continue
		}
		key, err := hlsGet(ctx, c, s.keyURL, 64)
		if err != nil || len(key) != aes.BlockSize {
			return errors.New("beatport: stream key unavailable")
		}
		keys[s.keyURL] = key
	}
	type result struct {
		data []byte
		err  error
	}
	results := make([]chan result, len(segments))
	for i := range results {
		results[i] = make(chan result, 1)
	}
	slots := make(chan struct{}, hlsParallel)
	go func() {
		for i, s := range segments {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				var data []byte
				var err error
				for attempt := 0; attempt < 3; attempt++ {
					if data, err = hlsGet(ctx, c, s.url, hlsMaxSegment); err == nil || ctx.Err() != nil {
						break
					}
				}
				if err == nil && s.keyURL != "" {
					data, err = decryptSegment(data, keys[s.keyURL], s.iv)
				}
				results[i] <- result{data, err}
			}()
		}
	}()
	for i := range segments {
		var r result
		select {
		case r = <-results[i]:
		case <-ctx.Done():
			return ctx.Err()
		}
		<-slots
		if r.err != nil {
			return r.err
		}
		if err := use(r.data); err != nil {
			return err
		}
	}
	return nil
}

// writeHLS joins the stream into one file. Fragmented MP4 is already a file once
// its init section leads; MPEG-TS and packed audio are remuxed.
func writeHLS(ctx context.Context, c *http.Client, init string, segments []hlsSegment, w io.WriteSeeker) error {
	if init != "" {
		head, err := hlsGet(ctx, c, init, hlsMaxSegment)
		if err == nil {
			_, err = w.Write(head)
		}
		if err != nil {
			return err
		}
		return fetchSegments(ctx, c, segments, func(b []byte) error { _, err := w.Write(b); return err })
	}
	m := &aacWriter{w: w}
	if err := m.start(); err != nil {
		return err
	}
	pid := -1
	var pending []byte
	err := fetchSegments(ctx, c, segments, func(b []byte) error {
		if len(b) > 0 && b[0] == 0x47 {
			var err error
			if b, err = tsAudio(b, &pid); err != nil {
				return err
			}
		}
		var err error
		pending, err = m.addADTS(append(pending, b...))
		return err
	})
	if err != nil {
		return err
	}
	return m.finish()
}

// tsAudio returns the payload of the first audio stream in MPEG-TS packets. pid
// remembers that stream across segments.
func tsAudio(b []byte, pid *int) ([]byte, error) {
	var out []byte
	for ; len(b) >= 188; b = b[188:] {
		p := b[:188]
		if p[0] != 0x47 {
			return nil, errors.New("beatport: invalid MPEG-TS segment")
		}
		id := int(p[1]&0x1f)<<8 | int(p[2])
		payload := p[4:]
		if p[3]&0x20 != 0 { // adaptation field
			if int(payload[0])+1 > len(payload) {
				continue
			}
			payload = payload[1+int(payload[0]):]
		}
		if p[3]&0x10 == 0 {
			continue
		}
		if p[1]&0x40 != 0 { // a PES packet starts; PAT and PMT sections never match
			if len(payload) < 9 || payload[0] != 0 || payload[1] != 0 || payload[2] != 1 || payload[3] < 0xc0 || payload[3] > 0xdf {
				continue
			}
			if *pid < 0 {
				*pid = id
			}
			if id != *pid || 9+int(payload[8]) > len(payload) {
				continue
			}
			payload = payload[9+int(payload[8]):]
		} else if id != *pid {
			continue
		}
		out = append(out, payload...)
	}
	return out, nil
}

var adtsRates = []uint32{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// aacWriter writes raw AAC frames as an MP4 file: ftyp, mdat, then moov.
type aacWriter struct {
	w        io.WriteSeeker
	config   []byte // AudioSpecificConfig
	rate     uint32
	channels uint16
	sizes    []uint32
	total    int64
}

func (m *aacWriter) start() error {
	_, err := m.w.Write(append(mp4Box("ftyp", []byte("M4A "), u32s(0), []byte("M4A mp42isom")), 0, 0, 0, 0, 'm', 'd', 'a', 't'))
	return err
}

// addADTS writes the whole ADTS frames in b and returns an unfinished last one.
func (m *aacWriter) addADTS(b []byte) ([]byte, error) {
	for len(b) > 0 {
		if len(b) >= 10 && string(b[:3]) == "ID3" { // packed audio carries timestamps this way
			n := 10 + (int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f))
			if b[5]&0x10 != 0 {
				n += 10
			}
			if n > len(b) {
				return b, nil
			}
			b = b[n:]
			continue
		}
		if len(b) < 7 {
			return b, nil
		}
		if b[0] != 0xff || b[1]&0xf6 != 0xf0 {
			return nil, errors.New("beatport: stream is not AAC")
		}
		header := 7
		if b[1]&1 == 0 {
			header = 9
		}
		n := int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5])>>5
		if n <= header {
			return nil, errors.New("beatport: invalid AAC frame")
		}
		if n > len(b) {
			return b, nil
		}
		if b[6]&3 != 0 {
			return nil, errors.New("beatport: unsupported AAC framing")
		}
		freq := b[2] >> 2 & 0xf
		if m.config == nil {
			if int(freq) >= len(adtsRates) {
				return nil, errors.New("beatport: invalid AAC sample rate")
			}
			profile, channels := b[2]>>6+1, (b[2]&1)<<2|b[3]>>6
			m.config = []byte{profile<<3 | freq>>1, (freq&1)<<7 | channels<<3}
			m.rate, m.channels = adtsRates[freq], uint16(channels)
		}
		frame := b[header:n]
		if m.total += int64(len(frame)); m.total > maxStreamBytes {
			return nil, errors.New("beatport: stream too large")
		}
		if _, err := m.w.Write(frame); err != nil {
			return nil, err
		}
		m.sizes = append(m.sizes, uint32(len(frame)))
		b = b[n:]
	}
	return nil, nil
}

func (m *aacWriter) finish() error {
	if len(m.sizes) == 0 {
		return errors.New("beatport: stream has no audio")
	}
	const ftypSize, mdatStart = 28, 28 + 8
	if _, err := m.w.Seek(ftypSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := m.w.Write(u32s(uint32(8 + m.total))); err != nil {
		return err
	}
	if _, err := m.w.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	n := uint32(len(m.sizes))
	duration := n * 1024 // one AAC frame is 1024 samples
	matrix := u32s(0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000)
	channels := m.channels
	if channels == 0 {
		channels = 2
	}
	config := append([]byte{0x05, byte(len(m.config))}, m.config...)
	decoder := append(append([]byte{0x04, byte(13 + len(config)), 0x40, 0x15, 0, 0, 0}, u32s(0, 0)...), config...)
	es := append(append([]byte{0x03, byte(3 + len(decoder) + 3), 0, 1, 0}, decoder...), 0x06, 0x01, 0x02)
	mp4a := mp4Box("mp4a", make([]byte, 6), u16s(1), make([]byte, 8), u16s(channels, 16, 0, 0), u32s(m.rate<<16), mp4Box("esds", u32s(0), es))
	stsz := mp4Box("stsz", u32s(0, 0, n), u32s(m.sizes...))
	stbl := mp4Box("stbl",
		mp4Box("stsd", u32s(0, 1), mp4a),
		mp4Box("stts", u32s(0, 1, n, 1024)),
		mp4Box("stsc", u32s(0, 1, 1, n, 1)),
		stsz,
		mp4Box("stco", u32s(0, 1, mdatStart)))
	minf := mp4Box("minf", mp4Box("smhd", u32s(0, 0)), mp4Box("dinf", mp4Box("dref", u32s(0, 1), mp4Box("url ", u32s(1)))), stbl)
	mdia := mp4Box("mdia",
		mp4Box("mdhd", u32s(0, 0, 0, m.rate, duration), u16s(0x55c4, 0)),
		mp4Box("hdlr", u32s(0, 0), []byte("soun"), make([]byte, 12), []byte("SoundHandler\x00")),
		minf)
	trak := mp4Box("trak", mp4Box("tkhd", u32s(7, 0, 0, 1, 0, duration, 0, 0, 0), u16s(0x100, 0), matrix, u32s(0, 0)), mdia)
	moov := mp4Box("moov", mp4Box("mvhd", u32s(0, 0, 0, m.rate, duration, 0x10000), u16s(0x100), make([]byte, 10), matrix, make([]byte, 24), u32s(2)), trak)
	_, err := m.w.Write(moov)
	return err
}

func mp4Box(kind string, parts ...[]byte) []byte {
	size := 8
	for _, p := range parts {
		size += len(p)
	}
	out := binary.BigEndian.AppendUint32(make([]byte, 0, size), uint32(size))
	out = append(out, kind...)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func u32s(v ...uint32) []byte {
	out := make([]byte, 0, 4*len(v))
	for _, x := range v {
		out = binary.BigEndian.AppendUint32(out, x)
	}
	return out
}

func u16s(v ...uint16) []byte {
	out := make([]byte, 0, 2*len(v))
	for _, x := range v {
		out = binary.BigEndian.AppendUint16(out, x)
	}
	return out
}
