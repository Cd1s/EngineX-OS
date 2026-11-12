//go:build arm64 && !purego

package stems

//go:noescape
func fma4RowNEON(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 *float32)

func fma4Row(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 []float32) {
	if n <= 0 {
		return
	}
	fma4RowNEON(n, a0, a1, a2, a3, &b[0], &c0[0], &c1[0], &c2[0], &c3[0])
}
