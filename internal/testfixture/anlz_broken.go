package testfixture

// MalformedANLZKinds are the sections ParseAnalysis reads, each made so that it refuses it (with
// its header and length right, so the parser reaches the body), and a section whose length runs
// past the file (#253 review 3).
var MalformedANLZKinds = []string{"PQTZ", "PCOB", "PCO2", "PWAV", "PWV2", "PWV3", "PWV4", "PWV5", "PWV7", "PSSI", "length"}

// MalformedANLZSection is a section of kind that rekordbox.ParseAnalysis refuses.
func MalformedANLZSection(kind string) []byte {
	sec := func(tag string, h, n int) []byte {
		b := make([]byte, n)
		copy(b, tag)
		be.PutUint32(b[4:], uint32(h))
		be.PutUint32(b[8:], uint32(n))
		return b
	}
	var b []byte
	switch kind {
	case "PQTZ": // 5 beats in no room
		b = sec(kind, 24, 24)
		be.PutUint32(b[20:], 5)
	case "PCOB": // one cue in no room
		b = sec(kind, 24, 24)
		be.PutUint16(b[18:], 1)
	case "PCO2":
		b = sec(kind, 20, 20)
		be.PutUint16(b[16:], 1)
	case "PWAV", "PWV2": // 100 one-byte entries in no room
		b = sec(kind, 20, 20)
		be.PutUint32(b[12:], 100)
	case "PWV3", "PWV4", "PWV5", "PWV7":
		b = sec(kind, 24, 24)
		be.PutUint32(b[12:], map[string]uint32{"PWV3": 1, "PWV4": 6, "PWV5": 2, "PWV7": 3}[kind])
		be.PutUint32(b[16:], 9)
	case "PSSI": // phrase entries narrower than a phrase
		b = sec(kind, 32, 32)
		be.PutUint32(b[12:], 10)
	case "length": // a section longer than the file
		b = sec("PXYZ", 12, 12)
		be.PutUint32(b[8:], 1<<30)
	default:
		panic("unknown malformed ANLZ kind " + kind)
	}
	return b
}

// WithANLZSectionFirst is the ANLZ file b with sec in front of its first section.
func WithANLZSectionFirst(b, sec []byte) []byte {
	h, n := int(be.Uint32(b[4:])), int(be.Uint32(b[8:]))
	out := append(append(append([]byte(nil), b[:h]...), sec...), b[h:n]...)
	be.PutUint32(out[8:], uint32(len(out)))
	return out
}
