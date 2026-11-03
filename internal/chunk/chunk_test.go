package chunk

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"testing"
)

func randBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestSplitReassembles(t *testing.T) {
	for _, n := range []int{0, 1, MinSize, MinSize + 1, MaxSize, MaxSize + 1, 3 << 20} {
		data := randBytes(1, n)
		chunks := Split(data)
		if got := bytes.Join(chunks, nil); !bytes.Equal(got, data) {
			t.Fatalf("n=%d: reassembled data differs", n)
		}
		for i, c := range chunks {
			if len(c) > MaxSize {
				t.Fatalf("chunk %d too big: %d", i, len(c))
			}
			if len(c) < MinSize && i != len(chunks)-1 {
				t.Fatalf("chunk %d too small: %d", i, len(c))
			}
		}
	}
}

func TestAverageSize(t *testing.T) {
	data := randBytes(2, 32<<20)
	chunks := Split(data)
	avg := len(data) / len(chunks)
	if avg < AvgSize/2 || avg > AvgSize*2 {
		t.Fatalf("average chunk size %d, want near %d", avg, AvgSize)
	}
}

func TestReaderMatchesSplit(t *testing.T) {
	data := randBytes(3, 5<<20)
	want := Split(data)
	// A reader that returns small, uneven reads.
	r := NewReader(&trickle{data: data})
	var i int
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c, want[i]) {
			t.Fatalf("chunk %d differs", i)
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("got %d chunks, want %d", i, len(want))
	}
}

type trickle struct {
	data []byte
	n    int
}

func (t *trickle) Read(p []byte) (int, error) {
	if len(t.data) == 0 {
		return 0, io.EOF
	}
	t.n++
	k := min(len(p), len(t.data), 1+t.n*7919%20000)
	copy(p, t.data[:k])
	t.data = t.data[k:]
	return k, nil
}

// An insertion near the start should leave most chunks unchanged.
func TestShiftResistance(t *testing.T) {
	data := randBytes(4, 8<<20)
	edited := append(append(append([]byte{}, data[:1000]...), []byte("inserted text")...), data[1000:]...)
	seen := map[[32]byte]bool{}
	for _, c := range Split(data) {
		seen[sha256.Sum256(c)] = true
	}
	chunks := Split(edited)
	var reused int
	for _, c := range chunks {
		if seen[sha256.Sum256(c)] {
			reused++
		}
	}
	if reused < len(chunks)-3 {
		t.Fatalf("only %d of %d chunks reused after a small insert", reused, len(chunks))
	}
}

func BenchmarkSplit(b *testing.B) {
	data := randBytes(5, 16<<20)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		Split(data)
	}
}
