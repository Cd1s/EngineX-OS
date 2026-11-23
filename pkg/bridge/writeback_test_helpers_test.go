package bridge_test

import (
	"context"

	"github.com/Cd1s/EngineX-OS/pkg/bridge"
	"github.com/Cd1s/EngineX-OS/pkg/enginedb"
)

func convertWriteBackAtTest(ctx context.Context, mount string) (enginedb.Result, error) {
	res, err := bridge.Convert(ctx, mount, enginedb.Options{})
	if err != nil {
		return res, err
	}
	if err := bridge.StampWriteBackForTest(mount); err != nil {
		return res, err
	}
	_, err = bridge.WriteBackCues(ctx, mount, bridge.WriteBackOptions{SkipPreparingCheck: true})
	return res, err
}
