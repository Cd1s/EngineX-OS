package main

import (
	dmxstudio "github.com/Cd1s/EngineX-OS/docs/mods/src/dmx-studio"
	joglogo "github.com/Cd1s/EngineX-OS/docs/mods/src/jog-logo-studio"
	midimapper "github.com/Cd1s/EngineX-OS/docs/mods/src/midi-controller-mapper"
	clubmode "github.com/Cd1s/EngineX-OS/docs/mods/src/minimal-club-mode"
	"github.com/Cd1s/EngineX-OS/docs/mods/src/modkit"
	sampler "github.com/Cd1s/EngineX-OS/docs/mods/src/sampler-pad-studio"
)

// features maps each official Mod id to its constructor.
var features = map[string]func(modkit.Env) (modkit.Feature, error){
	"dmx-studio":             dmxstudio.New,
	"jog-logo-studio":        joglogo.New,
	"sampler-pad-studio":     sampler.New,
	"minimal-club-mode":      clubmode.New,
	"midi-controller-mapper": midimapper.New,
}
