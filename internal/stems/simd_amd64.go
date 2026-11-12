//go:build amd64 && !purego

package stems

import "golang.org/x/sys/cpu"

var useAVX2FMA = cpu.X86.HasAVX2 && cpu.X86.HasFMA

//go:noescape
func fma4RowAVX2(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 *float32)

func fma4Row(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 []float32) {
	if n <= 0 {
		return
	}
	if useAVX2FMA {
		fma4RowAVX2(n, a0, a1, a2, a3, &b[0], &c0[0], &c1[0], &c2[0], &c3[0])
		return
	}
	fma4RowScalar(n, a0, a1, a2, a3, b, c0, c1, c2, c3)
}
