package feed

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// BlobToFloat32
// ---------------------------------------------------------------------------

func TestBlobToFloat32_Nil(t *testing.T) {
	if got := BlobToFloat32(nil); got != nil {
		t.Errorf("BlobToFloat32(nil) = %v, want nil", got)
	}
}

func TestBlobToFloat32_Empty(t *testing.T) {
	if got := BlobToFloat32([]byte{}); got != nil {
		t.Errorf("BlobToFloat32([]) = %v, want nil", got)
	}
}

func TestBlobToFloat32_OddLength(t *testing.T) {
	// Three bytes — not a multiple of 4, so must return nil.
	if got := BlobToFloat32([]byte{0x01, 0x02, 0x03}); got != nil {
		t.Errorf("BlobToFloat32(3-byte) = %v, want nil (non-multiple-of-4)", got)
	}
}

func TestBlobToFloat32_KnownValue(t *testing.T) {
	// 1.0 in IEEE-754 little-endian = 0x00 0x00 0x80 0x3F
	b := []byte{0x00, 0x00, 0x80, 0x3F}
	got := BlobToFloat32(b)
	if len(got) != 1 || got[0] != 1.0 {
		t.Errorf("BlobToFloat32(1.0 bytes) = %v, want [1.0]", got)
	}
}

// ---------------------------------------------------------------------------
// Float32ToBlob
// ---------------------------------------------------------------------------

func TestFloat32ToBlob_Nil(t *testing.T) {
	if got := Float32ToBlob(nil); len(got) != 0 {
		t.Errorf("Float32ToBlob(nil) len = %d, want 0", len(got))
	}
}

func TestFloat32ToBlob_LengthIsQuadrupled(t *testing.T) {
	in := []float32{0.0, 1.0, 2.0}
	if got := Float32ToBlob(in); len(got) != len(in)*4 {
		t.Errorf("Float32ToBlob len = %d, want %d", len(got), len(in)*4)
	}
}

// ---------------------------------------------------------------------------
// Roundtrip Float32ToBlob ↔ BlobToFloat32
// ---------------------------------------------------------------------------

func TestFloat32Roundtrip(t *testing.T) {
	cases := [][]float32{
		{0.0},
		{1.0, -1.0},
		{3.14, -3.14, 0.001, -0.001},
		{math.MaxFloat32, -math.MaxFloat32},
		{float32(math.SmallestNonzeroFloat32)},
	}
	for _, in := range cases {
		blob := Float32ToBlob(in)
		out := BlobToFloat32(blob)
		if len(out) != len(in) {
			t.Errorf("roundtrip len = %d, want %d (input %v)", len(out), len(in), in)
			continue
		}
		for i := range in {
			if out[i] != in[i] {
				t.Errorf("[%d] roundtrip = %v, want %v (input %v)", i, out[i], in[i], in)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// CosineSimilarity
// ---------------------------------------------------------------------------

func TestCosineSimilarity_Nil(t *testing.T) {
	if got := CosineSimilarity(nil, nil); got != 0 {
		t.Errorf("CosineSimilarity(nil,nil) = %v, want 0", got)
	}
}

func TestCosineSimilarity_Empty(t *testing.T) {
	if got := CosineSimilarity([]float32{}, []float32{}); got != 0 {
		t.Errorf("CosineSimilarity(empty,empty) = %v, want 0", got)
	}
}

func TestCosineSimilarity_LengthMismatch(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{1, 2}
	if got := CosineSimilarity(a, b); got != 0 {
		t.Errorf("CosineSimilarity(len mismatch) = %v, want 0", got)
	}
}

func TestCosineSimilarity_ZeroVectorA(t *testing.T) {
	z := []float32{0, 0, 0}
	v := []float32{1, 2, 3}
	if got := CosineSimilarity(z, v); got != 0 {
		t.Errorf("CosineSimilarity(zero,v) = %v, want 0", got)
	}
}

func TestCosineSimilarity_ZeroVectorB(t *testing.T) {
	v := []float32{1, 2, 3}
	z := []float32{0, 0, 0}
	if got := CosineSimilarity(v, z); got != 0 {
		t.Errorf("CosineSimilarity(v,zero) = %v, want 0", got)
	}
}

func TestCosineSimilarity_Identical(t *testing.T) {
	v := []float32{1, 2, 3}
	got := CosineSimilarity(v, v)
	if math.Abs(got-1.0) > 1e-6 {
		t.Errorf("CosineSimilarity(v,v) = %v, want ~1.0", got)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{-1, 0}
	got := CosineSimilarity(a, b)
	if math.Abs(got-(-1.0)) > 1e-6 {
		t.Errorf("CosineSimilarity(a,-a) = %v, want ~-1.0", got)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{0, 1}
	got := CosineSimilarity(a, b)
	if math.Abs(got) > 1e-6 {
		t.Errorf("CosineSimilarity(orthogonal) = %v, want ~0.0", got)
	}
}

func TestCosineSimilarity_KnownAngle45(t *testing.T) {
	// [1,0] vs [1,1] → cos(45°) = 1/√2 ≈ 0.7071
	a := []float32{1, 0}
	b := []float32{1, 1}
	got := CosineSimilarity(a, b)
	want := 1.0 / math.Sqrt2
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("CosineSimilarity(45deg) = %v, want %v", got, want)
	}
}

func TestCosineSimilarity_ScalingInvariant(t *testing.T) {
	// Scaling a vector should not change cosine similarity.
	a := []float32{1, 2, 3}
	b := []float32{2, 4, 6} // 2×a
	got := CosineSimilarity(a, b)
	if math.Abs(got-1.0) > 1e-6 {
		t.Errorf("CosineSimilarity(v, 2v) = %v, want ~1.0 (scale-invariant)", got)
	}
}
