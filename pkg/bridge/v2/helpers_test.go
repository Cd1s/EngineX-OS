package bridge

import "github.com/Cd1s/EngineX-OS/pkg/compat"

type resolverT = compat.Resolver

func newResolver(mount string) (*compat.Resolver, error) { return compat.NewResolver(mount) }
