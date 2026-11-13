package proto

import "strings"

// Board describes one supported EngineX OS target board.
type Board struct {
	Code   string
	Prefix string
	Name   string
	Gen2   bool
}

var gen1Boards = map[string]Board{
	"jp07": {Code: "jp07", Prefix: "SC5000", Name: "Denon DJ SC5000"},
	"jp08": {Code: "jp08", Prefix: "SC5000M", Name: "Denon DJ SC5000M"},
	"jp11": {Code: "jp11", Prefix: "PRIMEGO", Name: "Denon DJ PRIME GO"},
	"jp13": {Code: "jp13", Prefix: "SC6000", Name: "Denon DJ SC6000"},
	"jp14": {Code: "jp14", Prefix: "SC6000M", Name: "Denon DJ SC6000M"},
	"jc11": {Code: "jc11", Prefix: "PRIME4", Name: "Denon DJ PRIME 4"},
	"jc16": {Code: "jc16", Prefix: "PRIME2", Name: "Denon DJ PRIME 2"},
	"nh08": {Code: "nh08", Prefix: "MIXSTREAMPRO", Name: "Numark Mixstream Pro"},
}

var gen2Boards = map[string]Board{
	"jp21":  {Code: "jp21", Prefix: "SCLIVE4", Name: "Denon DJ SC LIVE 4", Gen2: true},
	"jp20":  {Code: "jp20", Prefix: "SCLIVE2", Name: "Denon DJ SC LIVE 2", Gen2: true},
	"jc11s": {Code: "jc11s", Prefix: "PRIME4PLUS", Name: "Denon DJ PRIME 4+", Gen2: true},
	"jp11s": {Code: "jp11s", Prefix: "PRIMEGOPLUS", Name: "Denon DJ PRIME GO+", Gen2: true},
	"nh08s": {Code: "nh08s", Prefix: "MIXSTREAMPROPLUS", Name: "Numark Mixstream Pro+", Gen2: true},
	"nh10":  {Code: "nh10", Prefix: "MIXSTREAMPROGO", Name: "Numark Mixstream Pro Go", Gen2: true},
	"jp24":  {Code: "jp24", Prefix: "PRIME4G2", Name: "Denon DJ PRIME 4 G2", Gen2: true},
	"rmz2":  {Code: "rmz2", Prefix: "SYSTEMONE", Name: "Rane SYSTEM ONE", Gen2: true},
}

// boardAliases maps marketing names some fastboot builds report as "product" (#129).
var boardAliases = map[string]string{
	"prime4plus": "jc11s",
	"prime4+":    "jc11s",
}

// gen1USBIDs maps allowlisted Gen-1 USB VID/PID pairs (packed as uint32(vid)<<16|uint32(pid))
// to their board code. Gen-1 fastboot may answer "getvar product" with "Variable not implemented".
var gen1USBIDs = map[uint32]string{
	0x15e4<<16 | 0xd006: "jp07",
	0x15e4<<16 | 0xd00a: "jp08",
	0x15e4<<16 | 0xd00c: "jp11",
	0x15e4<<16 | 0xd00d: "jp13",
	0x15e4<<16 | 0xd00e: "jp14",
	0x15e4<<16 | 0xd008: "jc11",
	0x15e4<<16 | 0xd00b: "jc16",
	0x15e4<<16 | 0x203f: "nh08",
}

// BoardCode normalizes a fastboot "product" variable or device-tree compatible
// token into a canonical board code while preserving #110/#129 rules.
func BoardCode(raw string) string {
	code := strings.ToLower(strings.TrimSpace(raw))
	code = strings.TrimPrefix(code, "inmusic,")
	if alias, ok := boardAliases[strings.ReplaceAll(code, " ", "")]; ok {
		return alias
	}
	if _, ok := gen2Boards[code]; ok {
		return code
	}
	if _, ok := gen1Boards[code]; ok {
		return code
	}
	if idx := strings.LastIndexAny(code, "-_/"); idx >= 0 {
		sub := strings.TrimPrefix(code[idx+1:], "inmusic,")
		if alias, ok := boardAliases[strings.ReplaceAll(sub, " ", "")]; ok {
			return alias
		}
		if _, ok := gen2Boards[sub]; ok {
			return sub
		}
		if _, ok := gen1Boards[sub]; ok {
			return sub
		}
	}
	return code
}

// LookupBoard resolves a normalized or raw product/board identifier to a known Board.
func LookupBoard(raw string) (Board, bool) {
	c := BoardCode(raw)
	if b, ok := gen2Boards[c]; ok {
		return b, true
	}
	b, ok := gen1Boards[c]
	return b, ok
}

// Gen1BoardByUSBID resolves an exact allowlisted Gen-1 VID/PID pair.
func Gen1BoardByUSBID(vid, pid uint16) (Board, bool) {
	code, ok := gen1USBIDs[uint32(vid)<<16|uint32(pid)]
	if !ok {
		return Board{}, false
	}
	b, ok := gen1Boards[code]
	return b, ok
}
