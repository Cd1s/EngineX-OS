//go:build !linux

package enginedb

import "os"

const openNoAtime = 0

func noReadahead(*os.File) {}
