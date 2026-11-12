package dmxstudio

import (
	"math"

	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
)

type phrasePalette struct {
	red, green, blue float64
}

var defaultPhrasePalettes = []phrasePalette{
	{255, 255, 255}, // neutral / warm white
	{0, 180, 255},   // cool cyan
	{255, 60, 180},  // magenta
	{40, 255, 130},  // emerald
}

func phrasePosition(beats, phraseBeats float64) (idx int, beatInPhrase float64) {
	if phraseBeats <= 0 {
		return 0, 0
	}
	q := math.Floor(beats / phraseBeats)
	idx = int(q)
	if idx < 0 {
		idx = 0
	}
	beatInPhrase = math.Mod(beats, phraseBeats)
	if beatInPhrase < 0 {
		beatInPhrase += phraseBeats
	}
	return idx, beatInPhrase
}

func validatePhraseScenes(raw any) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	list, ok := objects(raw)
	if !ok || len(list) > 32 {
		return nil, modkit.Bad("phraseScenes must be an array of at most 32 scene objects")
	}
	out := make([]any, len(list))
	sceneBounds := map[string][2]float64{
		"speed": {1, 64}, "width": {0, 32767}, "height": {0, 32767},
		"intensity": {0, 255}, "red": {0, 255}, "green": {0, 255}, "blue": {0, 255},
	}
	for i, s := range list {
		clean := map[string]any{}
		if ef, ok := s["effect"]; ok {
			switch ef {
			case "Circle", "8-Figure", "Pan-Tilt Sweep", "Static":
				clean["effect"] = ef
			default:
				return nil, modkit.Bad("unknown EFX in phraseScenes[%d]", i)
			}
		}
		for k, b := range sceneBounds {
			if v, ok := s[k]; ok {
				n, err := modkit.Number(v, b[0], b[1])
				if err != nil {
					return nil, err
				}
				clean[k] = n
			}
		}
		out[i] = clean
	}
	return out, nil
}

// applyPhrase returns cfg with phrase-synced scene overrides applied, without mutating cfg.
func applyPhrase(cfg map[string]any, totalBeats float64) map[string]any {
	pb, _ := modkit.Float(cfg["phraseBeats"])
	if pb <= 0 {
		return cfg
	}
	idx, _ := phrasePosition(totalBeats, pb)
	eff := make(map[string]any, len(cfg))
	for k, v := range cfg {
		eff[k] = v
	}
	scenes, _ := objects(cfg["phraseScenes"])
	if len(scenes) > 0 {
		scene := scenes[idx%len(scenes)]
		for k, v := range scene {
			eff[k] = v
		}
		return eff
	}
	pal := defaultPhrasePalettes[idx%len(defaultPhrasePalettes)]
	eff["red"], eff["green"], eff["blue"] = pal.red, pal.green, pal.blue
	return eff
}
