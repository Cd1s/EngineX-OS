//go:build !linux

package bridge

import "context"

func watchMountChanges(context.Context) (<-chan struct{}, func()) {
	return nil, func() {}
}
