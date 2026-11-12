package stems

import (
	"runtime"
	"sync"
)

func fma4RowScalar(n int, a0, a1, a2, a3 float32, b, c0, c1, c2, c3 []float32) {
	_ = b[n-1]
	_ = c0[n-1]
	_ = c1[n-1]
	_ = c2[n-1]
	_ = c3[n-1]
	for j := 0; j < n; j++ {
		bv := b[j]
		c0[j] += a0 * bv
		c1[j] += a1 * bv
		c2[j] += a2 * bv
		c3[j] += a3 * bv
	}
}

func defaultWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		return 1
	}
	if n > 16 {
		return 16
	}
	return n
}

// sgemmAdd computes C[M x N] += alpha * A[M x K] * B[K x N] (row-major).
func sgemmAdd(M, N, K int, alpha float32, A, B, C []float32, workers int) {
	if M <= 0 || N <= 0 || K <= 0 {
		return
	}
	if workers <= 0 {
		workers = defaultWorkers()
	}
	if workers <= 1 || M < 16 || M*N*K < 65536 {
		sgemmRows(0, M, N, K, alpha, A, B, C)
		return
	}
	chunk := ((M + workers - 1) / workers)
	if chunk < 4 {
		chunk = 4
	}
	chunk = (chunk + 3) &^ 3
	var wg sync.WaitGroup
	for r0 := 0; r0 < M; r0 += chunk {
		r1 := r0 + chunk
		if r1 > M {
			r1 = M
		}
		wg.Add(1)
		go func(r0, r1 int) {
			defer wg.Done()
			sgemmRows(r0, r1, N, K, alpha, A, B, C)
		}(r0, r1)
	}
	wg.Wait()
}

func sgemmRows(r0, r1, N, K int, alpha float32, A, B, C []float32) {
	const tileK = 64
	for kk := 0; kk < K; kk += tileK {
		kend := kk + tileK
		if kend > K {
			kend = K
		}
		i := r0
		for ; i+4 <= r1; i += 4 {
			c0 := C[i*N : (i+1)*N]
			c1 := C[(i+1)*N : (i+2)*N]
			c2 := C[(i+2)*N : (i+3)*N]
			c3 := C[(i+3)*N : (i+4)*N]
			a0r := A[i*K : (i+1)*K]
			a1r := A[(i+1)*K : (i+2)*K]
			a2r := A[(i+2)*K : (i+3)*K]
			a3r := A[(i+3)*K : (i+4)*K]
			for k := kk; k < kend; k++ {
				a0 := a0r[k] * alpha
				a1 := a1r[k] * alpha
				a2 := a2r[k] * alpha
				a3 := a3r[k] * alpha
				if a0 == 0 && a1 == 0 && a2 == 0 && a3 == 0 {
					continue
				}
				fma4Row(N, a0, a1, a2, a3, B[k*N:(k+1)*N], c0, c1, c2, c3)
			}
		}
		for ; i < r1; i++ {
			cr := C[i*N : (i+1)*N]
			ar := A[i*K : (i+1)*K]
			for k := kk; k < kend; k++ {
				a := ar[k] * alpha
				if a == 0 {
					continue
				}
				br := B[k*N : (k+1)*N]
				for j := 0; j < N; j++ {
					cr[j] += a * br[j]
				}
			}
		}
	}
}
