package audioheader

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

func TestID3v24FooterAndAudioPrefix(t *testing.T) {
	header := []byte{'I', 'D', '3', 4, 0, 16, 0, 0, 0, 3}
	footer := append([]byte("3DI"), header[3:]...)
	data := append(append(append([]byte{}, header...), []byte("tag")...), footer...)
	data = append(data, []byte("fLaC")...)
	r := bufio.NewReader(bytes.NewReader(data))
	if err := SkipID3(r); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "fLaC" {
		t.Fatalf("wrong audio position: %q", rest)
	}
	data[13] = 'X'
	if err := SkipID3(bufio.NewReader(bytes.NewReader(data))); err == nil {
		t.Fatal("invalid footer accepted")
	}
}

func TestSkipID3MultipleTagsAndPostTagZeroPadding(t *testing.T) {
	tag1 := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 2, 'A', 'B'}
	pad := []byte{0, 0, 0, 0}
	tag2 := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, 1, 'C'}
	stream := append(append(append(append([]byte{}, tag1...), pad...), tag2...), pad...)
	stream = append(stream, []byte("fLaC")...)

	r := bufio.NewReader(bytes.NewReader(stream))
	if err := SkipID3(r); err != nil {
		t.Fatalf("SkipID3 failed on multi-tag padded stream: %v", err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "fLaC" {
		t.Fatalf("got %q, want fLaC", rest)
	}

	// More than 8 consecutive ID3v2 tags must be rejected rather than leaked downstream.
	var nine []byte
	for i := 0; i < 9; i++ {
		nine = append(nine, 'I', 'D', '3', 4, 0, 0, 0, 0, 0, 0)
	}
	nine = append(nine, []byte("fLaC")...)
	if err := SkipID3(bufio.NewReader(bytes.NewReader(nine))); err == nil {
		t.Fatal("expected error on >8 ID3 headers")
	}
}
