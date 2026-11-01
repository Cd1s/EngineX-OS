package analyzer

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/Cd1s/sc5000-custom-os/internal/audioheader"
	"github.com/hajimehoshi/go-mp3"
	"github.com/mewkiz/flac"
)

// Audio 是解码后的单声道 PCM（各声道平均），值域约 [-1, 1]。
type Audio struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	// Format 为 "wav"、"flac" 或 "mp3"。
	Format  string
	Samples []float32
}

// Duration 返回音频时长（秒）。
func (a *Audio) Duration() float64 {
	if a.SampleRate == 0 {
		return 0
	}
	return float64(len(a.Samples)) / float64(a.SampleRate)
}

// DecodeFile 按文件头（而不是扩展名）识别 WAV/FLAC/MP3 并解码。
func DecodeFile(path string) (*Audio, error) {
	return decodeFile(path, nil)
}

type tickReader struct {
	r      io.Reader
	ticker Ticker
}

func (r tickReader) Read(p []byte) (int, error) {
	if r.ticker != nil {
		if err := r.ticker.Tick(); err != nil {
			return 0, err
		}
	}
	return r.r.Read(p)
}

// MaxDecodedSamples bounds mono PCM to 128 MiB (about 12 minutes at 44.1kHz).
// Oversized tracks fail explicitly rather than exhausting the player's RAM.
const MaxDecodedSamples = 32 * 1024 * 1024

func decodeFile(path string, ticker Ticker) (*Audio, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(tickReader{f, ticker}, 1<<16)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	head, _ := br.Peek(12)
	switch {
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return DecodeWAV(br)
	case len(head) >= 4 && string(head[0:4]) == "fLaC":
		return DecodeFLAC(br)
	case len(head) >= 3 && string(head[0:3]) == "ID3",
		len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return DecodeMP3(br)
	}
	return nil, fmt.Errorf("analyzer: unsupported audio format: %s", path)
}

// WAVE 格式码。
const (
	wavFormatPCM        = 0x0001
	wavFormatFloat      = 0x0003
	wavFormatExtensible = 0xFFFE
)

// DecodeWAV 解码 PCM 8/16/24/32 bit 与 IEEE float 32/64 bit 的 WAV，
// 包括 WAVEFORMATEXTENSIBLE 头（子格式 GUID 的前两字节即格式码）。
func DecodeWAV(r io.Reader) (*Audio, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, errors.New("analyzer: not a RIFF/WAVE file")
	}
	var (
		format, channels, bits int
		sampleRate             int
		haveFmt                bool
	)
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, fmt.Errorf("analyzer: wav: missing data chunk: %w", err)
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch id {
		case "fmt ":
			if size < 16 || size > 65536 {
				return nil, errors.New("analyzer: wav: fmt chunk too short")
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, err
			}
			format = int(binary.LittleEndian.Uint16(buf[0:2]))
			channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			sampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			bits = int(binary.LittleEndian.Uint16(buf[14:16]))
			if format == wavFormatExtensible {
				if size < 40 {
					return nil, errors.New("analyzer: wav: extensible fmt chunk too short")
				}
				format = int(binary.LittleEndian.Uint16(buf[24:26]))
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, errors.New("analyzer: wav: data before fmt")
			}
			// 有些编码器把 data 长度写成 0 或 0xFFFFFFFF（流式写入），此时读到 EOF。
			var src io.Reader = r
			if size > 0 && size != 0xFFFFFFFF {
				src = io.LimitReader(r, size)
			}
			a, err := decodeWAVData(src, format, channels, sampleRate, bits)
			if err == nil && size > 0 && size != 0xFFFFFFFF && int64(len(a.Samples))*int64(channels*bits/8) != size {
				return nil, errors.New("analyzer: truncated WAV data")
			}
			return a, err
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, err
			}
			continue
		}
		if size%2 == 1 {
			if _, err := io.CopyN(io.Discard, r, 1); err != nil {
				return nil, err
			}
		}
	}
}

func decodeWAVData(r io.Reader, format, channels, sampleRate, bits int) (*Audio, error) {
	if channels <= 0 || channels > 8 || sampleRate < 8000 || sampleRate > 192000 {
		return nil, errors.New("analyzer: wav: invalid fmt")
	}
	var conv func([]byte) float64
	switch {
	case format == wavFormatPCM && bits == 8:
		conv = func(b []byte) float64 { return (float64(b[0]) - 128) / 128 }
	case format == wavFormatPCM && bits == 16:
		conv = func(b []byte) float64 { return float64(int16(binary.LittleEndian.Uint16(b))) / 32768 }
	case format == wavFormatPCM && bits == 24:
		conv = func(b []byte) float64 {
			v := int32(uint32(b[0])<<8|uint32(b[1])<<16|uint32(b[2])<<24) >> 8
			return float64(v) / 8388608
		}
	case format == wavFormatPCM && bits == 32:
		conv = func(b []byte) float64 { return float64(int32(binary.LittleEndian.Uint32(b))) / 2147483648 }
	case format == wavFormatFloat && bits == 32:
		conv = func(b []byte) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))) }
	case format == wavFormatFloat && bits == 64:
		conv = func(b []byte) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(b)) }
	default:
		return nil, fmt.Errorf("analyzer: wav: unsupported format %#x / %d bit", format, bits)
	}
	bps := bits / 8
	frame := bps * channels
	a := &Audio{SampleRate: sampleRate, Channels: channels, BitsPerSample: bits, Format: "wav"}
	buf := make([]byte, frame*4096)
	inv := 1 / float64(channels)
	for {
		n, err := io.ReadFull(r, buf)
		if n%frame != 0 {
			return nil, errors.New("analyzer: truncated WAV frame")
		}
		if len(a.Samples)+n/frame > MaxDecodedSamples {
			return nil, errors.New("analyzer: PCM memory limit exceeded")
		}
		n -= n % frame
		for off := 0; off < n; off += frame {
			sum := 0.0
			for c := 0; c < channels; c++ {
				v := conv(buf[off+c*bps:])
				if math.IsNaN(v) || math.IsInf(v, 0) {
					return nil, errors.New("analyzer: wav: non-finite sample")
				}
				// Average before summing to avoid overflowing finite float64 PCM.
				sum += v * inv
			}
			mono := float32(sum)
			if math.IsInf(float64(mono), 0) {
				return nil, errors.New("analyzer: wav: sample exceeds float32 range")
			}
			a.Samples = append(a.Samples, mono)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return a, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// DecodeFLAC 用纯 Go 的 mewkiz/flac 解码。
func DecodeFLAC(r io.Reader) (*Audio, error) {
	br := bufio.NewReader(r)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	stream, err := flac.New(br)
	if err != nil {
		return nil, fmt.Errorf("analyzer: flac: %w", err)
	}
	info := stream.Info
	if info.BitsPerSample < 8 || info.BitsPerSample > 32 || info.NChannels < 1 || info.NChannels > 8 {
		return nil, errors.New("analyzer: flac: invalid sample format")
	}
	if info.NSamples > MaxDecodedSamples || info.SampleRate < 8000 || info.SampleRate > 192000 {
		return nil, errors.New("analyzer: FLAC exceeds sample/rate limit")
	}
	a := &Audio{
		SampleRate:    int(info.SampleRate),
		Channels:      int(info.NChannels),
		BitsPerSample: int(info.BitsPerSample),
		Format:        "flac",
	}
	if info.NSamples > 0 {
		a.Samples = make([]float32, 0, info.NSamples)
	}
	scale := 1 / (float64(int64(1)<<(info.BitsPerSample-1)) * float64(info.NChannels))
	for {
		fr, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("analyzer: flac: %w", err)
		}
		// A zero frame field inherits STREAMINFO; ParseNext does not resolve it.
		if fr.BitsPerSample == 0 {
			fr.BitsPerSample = info.BitsPerSample
		}
		if fr.SampleRate == 0 {
			fr.SampleRate = info.SampleRate
		}
		if fr.BitsPerSample != info.BitsPerSample || fr.SampleRate != info.SampleRate || fr.Channels.Count() != int(info.NChannels) {
			return nil, errors.New("analyzer: flac: frame format differs from STREAMINFO")
		}
		if err := fr.Parse(); err != nil {
			return nil, fmt.Errorf("analyzer: flac: %w", err)
		}
		n := fr.Subframes[0].NSamples
		if len(a.Samples)+n > MaxDecodedSamples {
			return nil, errors.New("analyzer: PCM memory limit exceeded")
		}
		for i := 0; i < n; i++ {
			var sum int64
			for _, sf := range fr.Subframes {
				sum += int64(sf.Samples[i])
			}
			a.Samples = append(a.Samples, float32(float64(sum)*scale))
		}
	}
	if info.NSamples != 0 && uint64(len(a.Samples)) != info.NSamples {
		return nil, errors.New("analyzer: truncated FLAC samples")
	}
	return a, nil
}

// DecodeMP3 用纯 Go 的 go-mp3 解码（Deemix 回退到 MP3 320k 时使用）。
func DecodeMP3(r io.Reader) (*Audio, error) {
	// go-mp3 不跳过 ID3v2 标签，这里手动跳过。
	br := bufio.NewReader(r)
	if err := audioheader.SkipID3(br); err != nil {
		return nil, err
	}
	dec, err := mp3.NewDecoder(br)
	if err != nil {
		return nil, fmt.Errorf("analyzer: mp3: %w", err)
	}
	sampleRate := dec.SampleRate()
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, errors.New("analyzer: mp3: invalid sample rate")
	}
	a := &Audio{SampleRate: sampleRate, Channels: 2, BitsPerSample: 16, Format: "mp3"}
	buf := make([]byte, 4*4096)
	for {
		n, err := io.ReadFull(dec, buf)
		if len(a.Samples)+n/4 > MaxDecodedSamples {
			return nil, errors.New("analyzer: PCM memory limit exceeded")
		}
		n -= n % 4
		for off := 0; off < n; off += 4 {
			l := int16(binary.LittleEndian.Uint16(buf[off:]))
			rr := int16(binary.LittleEndian.Uint16(buf[off+2:]))
			a.Samples = append(a.Samples, float32((float64(l)+float64(rr))/65536))
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return a, nil
		}
		if err != nil {
			return nil, fmt.Errorf("analyzer: mp3: %w", err)
		}
	}
}
