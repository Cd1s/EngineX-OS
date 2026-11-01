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
