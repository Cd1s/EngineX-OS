package azimg

import "strings"

var gen1ByCompatible = map[string]Model{
	"inmusic,jp07": {
		Compatible: "inmusic,jp07", Board: "jp07", Code: "JP07", Prefix: "SC5000",
		Name: "SC5000", FullName: "Denon DJ SC5000",
	},
	"inmusic,jp08": {
		Compatible: "inmusic,jp08", Board: "jp08", Code: "JP08", Prefix: "SC5000M",
		Name: "SC5000M", FullName: "Denon DJ SC5000M",
	},
	"inmusic,jp11": {
		Compatible: "inmusic,jp11", Board: "jp11", Code: "JP11", Prefix: "PRIMEGO",
		Name: "PRIME GO", FullName: "Denon DJ PRIME GO",
	},
	"inmusic,jp13": {
		Compatible: "inmusic,jp13", Board: "jp13", Code: "JP13", Prefix: "SC6000",
		Name: "SC6000", FullName: "Denon DJ SC6000",
	},
	"inmusic,jp14": {
		Compatible: "inmusic,jp14", Board: "jp14", Code: "JP14", Prefix: "SC6000M",
		Name: "SC6000M", FullName: "Denon DJ SC6000M",
	},
	"inmusic,jc11": {
		Compatible: "inmusic,jc11", Board: "jc11", Code: "JC11", Prefix: "PRIME4",
		Name: "PRIME 4", FullName: "Denon DJ PRIME 4",
	},
	"inmusic,jc16": {
		Compatible: "inmusic,jc16", Board: "jc16", Code: "JC16", Prefix: "PRIME2",
		Name: "PRIME 2", FullName: "Denon DJ PRIME 2",
	},
	"inmusic,nh08": {
		Compatible: "inmusic,nh08", Board: "nh08", Code: "NH08", Prefix: "MIXSTREAMPRO",
		Name: "MIXSTREAM PRO", FullName: "Numark Mixstream Pro",
	},
}

var gen2ByPrefix = map[string]Model{
	"SCLIVE4": {
		Compatible: "inmusic,jp21", Board: "jp21", Code: "JP21", Prefix: "SCLIVE4",
		Name: "Denon DJ SC LIVE 4", FullName: "Denon DJ SC LIVE 4", Gen2: true,
	},
	"SCLIVE2": {
		Compatible: "inmusic,jp20", Board: "jp20", Code: "JP20", Prefix: "SCLIVE2",
		Name: "Denon DJ SC LIVE 2", FullName: "Denon DJ SC LIVE 2", Gen2: true,
	},
	"PRIME4PLUS": {
		Compatible: "inmusic,jc11s", Board: "jc11s", Code: "JC11S", Prefix: "PRIME4PLUS",
		Name: "Denon DJ PRIME 4+", FullName: "Denon DJ PRIME 4+", Gen2: true,
	},
	"PRIMEGOPLUS": {
		Compatible: "inmusic,jp11s", Board: "jp11s", Code: "JP11S", Prefix: "PRIMEGOPLUS",
		Name: "Denon DJ PRIME GO+", FullName: "Denon DJ PRIME GO+", Gen2: true,
	},
	"MIXSTREAMPROPLUS": {
		Compatible: "inmusic,nh08s", Board: "nh08s", Code: "NH08S", Prefix: "MIXSTREAMPROPLUS",
		Name: "Numark Mixstream Pro+", FullName: "Numark Mixstream Pro+", Gen2: true,
	},
	"MIXSTREAMPROGO": {
		Compatible: "inmusic,nh10", Board: "nh10", Code: "NH10", Prefix: "MIXSTREAMPROGO",
		Name: "Numark Mixstream Pro Go", FullName: "Numark Mixstream Pro Go", Gen2: true,
	},
	"PRIME4G2": {
		Compatible: "inmusic,jp24", Board: "jp24", Code: "JP24", Prefix: "PRIME4G2",
		Name: "Denon DJ PRIME 4 G2", FullName: "Denon DJ PRIME 4 G2", Gen2: true, AZ04: true,
	},
	"SYSTEMONE": {
		Compatible: "inmusic,rmz2", Board: "rmz2", Code: "RMZ2", Prefix: "SYSTEMONE",
		Name: "Rane SYSTEM ONE", FullName: "Rane SYSTEM ONE", Gen2: true, AZ04: true,
	},
}

// LookupGen1Compatible resolves an exact AZ01 device-tree compatible string (e.g. "inmusic,jp07"),
// matching az01_packer.py's models_of() exact-key lookup.
func LookupGen1Compatible(compat string) (Model, bool) {
	m, ok := gen1ByCompatible[compat]
	return m, ok
}

// LookupGen2Prefix resolves a Gen-2 release asset prefix (e.g. "SCLIVE4", "SYSTEMONE").
func LookupGen2Prefix(prefix string) (Model, bool) {
	m, ok := gen2ByPrefix[strings.ToUpper(strings.TrimSpace(prefix))]
	return m, ok
}

var boardAliases = map[string]string{
	"prime4plus": "jc11s",
	"prime4+":    "jc11s",
}

func normalizeBoardCode(id string) string {
	code := strings.ToLower(strings.TrimSpace(id))
	code = strings.TrimPrefix(code, "inmusic,")
	if alias, ok := boardAliases[strings.ReplaceAll(code, " ", "")]; ok {
		return alias
	}
	if idx := strings.LastIndexAny(code, "-_/"); idx >= 0 {
		sub := strings.TrimPrefix(code[idx+1:], "inmusic,")
		if alias, ok := boardAliases[sub]; ok {
			return alias
		}
		return sub
	}
	return code
}

// LookupModel resolves a board code, compatible string, platform-prefixed fastboot
// product string (e.g. "rk3288-az05-jp11s", "prime4+"), or release prefix to a Model.
func LookupModel(id string) (Model, bool) {
	key := strings.TrimSpace(id)
	if key == "" {
		return Model{}, false
	}
	if m, ok := LookupGen1Compatible(key); ok {
		return m, true
	}
	if m, ok := LookupGen2Prefix(key); ok {
		return m, true
	}
	lower := strings.ToLower(strings.TrimPrefix(strings.ToLower(key), "inmusic,"))
	norm := normalizeBoardCode(key)
	for _, m := range gen1ByCompatible {
		if m.Board == lower || m.Board == norm || strings.EqualFold(m.Prefix, key) {
			return m, true
		}
	}
	for _, m := range gen2ByPrefix {
		if m.Board == lower || m.Board == norm || strings.EqualFold(m.Prefix, key) {
			return m, true
		}
	}
	return Model{}, false
}
