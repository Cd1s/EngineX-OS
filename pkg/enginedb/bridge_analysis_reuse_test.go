package enginedb

import (
	"bytes"
	"fmt"
	"testing"
)

func TestPooledAnalysisWritersKeepRowsIndependent(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			plain := bytes.Repeat([]byte(fmt.Sprint("row-", i)), 1000)
			packed, err := packAnalysis(plain)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = packAnalysis([]byte("another column")); err != nil {
				t.Fatal(err)
			}
			got, err := UnpackAnalysis(packed, len(plain))
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("pooled writer changed a saved row: %v", err)
			}
		})
	}
}
