//go:build ignore

// Command gen_testaudio synthesizes deterministic non-copyright test tracks
// for the shadow-twin sandbox using internal/testaudio.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"math"
	"os"
	"path/filepath"

	"github.com/Cd1s/EngineX-OS/internal/testaudio"
)

const outRate = 44100 // 2x testaudio.Rate (22050 Hz); 44.1 kHz stereo avoids Engine's mono 22050 Hz short-read bug.

func writeStereoWAV(path string, seconds int, bpm float64, rootHz float64) error {
	base := testaudio.Song(seconds)
	totalFrames := len(base) * 2 // 44100 Hz frames
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)

	dataBytes := uint32(totalFrames * 4) // 2 channels * 2 bytes/sample
	w.WriteString("RIFF")
	binary.Write(w, binary.LittleEndian, uint32(36)+dataBytes)
	w.WriteString("WAVEfmt ")
	binary.Write(w, binary.LittleEndian, uint32(16))
	binary.Write(w, binary.LittleEndian, uint16(1))         // PCM
	binary.Write(w, binary.LittleEndian, uint16(2))         // 2 channels (stereo)
	binary.Write(w, binary.LittleEndian, uint32(outRate))   // 44100 Hz
	binary.Write(w, binary.LittleEndian, uint32(outRate*4)) // byte rate
	binary.Write(w, binary.LittleEndian, uint16(4))         // block align
	binary.Write(w, binary.LittleEndian, uint16(16))        // bits per sample
	w.WriteString("data")
	binary.Write(w, binary.LittleEndian, dataBytes)

	beatPeriod := 60.0 / bpm
	var buf [4]byte
	for i := 0; i < totalFrames; i++ {
		t := float64(i) / outRate
		beatPos := math.Mod(t, beatPeriod) / beatPeriod
		barBeat := int(t/beatPeriod) % 16

		// Low band (blue in Denon 3-band): punchy kick on beat + rolling sub-bass (55-110 Hz).
		kick := 0.0
		if beatPos < 0.22 {
			env := math.Exp(-beatPos * 16.0)
			kick = 0.55 * env * math.Sin(2*math.Pi*(58.0+42.0*math.Exp(-beatPos*35.0))*t)
		}
		subEnv := 0.35 + 0.45*math.Sin(math.Pi*beatPos)
		if barBeat >= 14 {
			subEnv *= 0.25
		}
		bass := subEnv * (0.42*math.Sin(2*math.Pi*65.41*t) + 0.25*math.Sin(2*math.Pi*98.0*t))

		// Mid band (green in Denon 3-band): chord stabs modulated with internal/testaudio.Song.
		midGate := 0.22 + 0.45*math.Pow(math.Sin(math.Pi*beatPos), 2)
		if barBeat >= 14 {
			midGate *= 0.35
		}
		baseSample := float64(base[i/2]) / 32767.0
		mid := baseSample*0.28*midGate +
			0.14*midGate*math.Sin(2*math.Pi*rootHz*t) +
			0.10*midGate*math.Sin(2*math.Pi*rootHz*1.25*t)

		// High band (white in Denon 3-band): crisp 16th-note hi-hat + off-beat open hat.
		subPos := math.Mod(beatPos*4.0, 1.0)
		hatAmp := 0.08
		if int(beatPos*4.0) == 2 {
			hatAmp = 0.22
		}
		hat := hatAmp * math.Exp(-subPos*20.0) *
			(math.Sin(2*math.Pi*4800.0*t) + 0.5*math.Sin(2*math.Pi*6400.0*t))

		v := kick + bass + mid + hat
		if v > 0.96 {
			v = 0.96
		} else if v < -0.96 {
			v = -0.96
		}
		s := int16(v * 32767.0)
		binary.LittleEndian.PutUint16(buf[0:2], uint16(s))
		binary.LittleEndian.PutUint16(buf[2:4], uint16(s))
		w.Write(buf[:])
	}
	return w.Flush()
}

func main() {
	outDir := flag.String("out", "/var/tmp/shadow-twin/music", "output directory for synthesized WAV files")
	seconds := flag.Int("seconds", 90, "duration in seconds")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		panic(err)
	}
	if err := writeStereoWAV(filepath.Join(*outDir, "track-a.wav"), *seconds, 128.0, 220.0); err != nil {
		panic(err)
	}
	if err := writeStereoWAV(filepath.Join(*outDir, "track-b.wav"), *seconds, 132.0, 293.66); err != nil {
		panic(err)
	}
}
