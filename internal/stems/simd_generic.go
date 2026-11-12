//go:build (!amd64 && !arm64) || purego

package stems

func fma4Row(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 []float32) {
	if n <= 0 {
		return
	}
	fma4RowScalar(n, a0, a1, a2, a3, b, c0, c1, c2, c3)
}
