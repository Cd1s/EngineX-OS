package webui

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
)

func mdnsQ(typ uint16, labels ...string) []byte {
	q := mdnsLabels([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}, labels...)
	return binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(q, typ), 1)
}

func TestSpotifyMDNSAnswersOurNamesOnly(t *testing.T) {
	svc := []string{"_spotify-connect", "_tcp", "local"}
	if !spotifyMDNSWants(mdnsQ(mdnsPTR, svc...), "deck") {
		t.Fatal("PTR query for the service ignored")
	}
	if !spotifyMDNSWants(mdnsQ(mdnsA, "deck", "local"), "deck") {
		t.Fatal("A query for the host ignored")
	}
	if spotifyMDNSWants(mdnsQ(mdnsPTR, "_airplay", "_tcp", "local"), "deck") {
		t.Fatal("answered another service")
	}
	resp := mdnsQ(mdnsPTR, svc...)
	resp[2] = 0x84
	if spotifyMDNSWants(resp, "deck") {
		t.Fatal("answered a response")
	}
}

func TestSpotifyMDNSAnswerCarriesPortTXTAndAddress(t *testing.T) {
	m := spotifyMDNSAnswer("deck", net.IPv4(192, 168, 1, 20))
	if binary.BigEndian.Uint16(m[6:]) != 4 || m[2] != 0x84 {
		t.Fatalf("header % x", m[:12])
	}
	for _, want := range []string{"CPath=/spotify-connect", "VERSION=1.0", "Stack=SP", "EngineX OS", "\x1f\x90", "\xc0\xa8\x01\x14"} {
		if !bytes.Contains(m, []byte(want)) {
			t.Errorf("answer lacks %q", want)
		}
	}
}
