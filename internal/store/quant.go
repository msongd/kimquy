package store

import (
	"fmt"
	"math"
)

// Quantize truncates v to dim components (Matryoshka), re-normalizes it to unit
// length and int8-quantizes it with a per-vector scale, writing into dst.
// It rejects vectors that are too short, non-finite, or all zero.
func Quantize(dst []int8, v []float32, dim int) (scale float32, err error) {
	if len(dst) != dim {
		return 0, fmt.Errorf("quantize: dst len %d != dim %d", len(dst), dim)
	}
	if len(v) < dim {
		return 0, fmt.Errorf("quantize: vector has %d dims, need at least %d", len(v), dim)
	}
	var sum float64
	for _, x := range v[:dim] {
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, fmt.Errorf("quantize: non-finite component %v", x)
		}
		sum += f * f
	}
	if sum == 0 {
		return 0, fmt.Errorf("quantize: zero vector")
	}
	inv := 1 / math.Sqrt(sum)
	var maxAbs float64
	for _, x := range v[:dim] {
		maxAbs = max(maxAbs, math.Abs(float64(x)*inv))
	}
	s := maxAbs / 127
	for i, x := range v[:dim] {
		q := math.Round(float64(x) * inv / s)
		dst[i] = int8(max(-127, min(127, q)))
	}
	return float32(s), nil
}

// dot is the int8 inner product, unrolled; the result fits easily in int32
// (256 * 127 * 127 < 2^23).
func dot(a, b []int8) int32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 int32
	i := 0
	for ; i+8 <= len(a); i += 8 {
		s0 += int32(a[i])*int32(b[i]) + int32(a[i+4])*int32(b[i+4])
		s1 += int32(a[i+1])*int32(b[i+1]) + int32(a[i+5])*int32(b[i+5])
		s2 += int32(a[i+2])*int32(b[i+2]) + int32(a[i+6])*int32(b[i+6])
		s3 += int32(a[i+3])*int32(b[i+3]) + int32(a[i+7])*int32(b[i+7])
	}
	for ; i < len(a); i++ {
		s0 += int32(a[i]) * int32(b[i])
	}
	return s0 + s1 + s2 + s3
}
