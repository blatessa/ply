// Package chunk splits data into content-defined chunks using FastCDC.
//
// Boundaries depend only on nearby content, so an edit changes only the chunks
// around it and the rest of the file deduplicates against earlier versions.
// The parameters are part of the storage format: changing them changes chunk
// boundaries (data stays readable, but dedup against old versions is lost).
package chunk

import (
	"io"
	"math/bits"
)

const (
	MinSize = 8 << 10   // 8 KiB
	AvgSize = 32 << 10  // 32 KiB
	MaxSize = 128 << 10 // 128 KiB
)

// Normalized chunking masks (FastCDC level 2): stricter before the average
// size, looser after, which tightens the chunk size distribution.
var (
	maskS = spreadMask(bits.Len(AvgSize) - 1 + 2)
	maskL = spreadMask(bits.Len(AvgSize) - 1 - 2)
)

// gear is a fixed table of pseudo-random values, generated deterministically
// so boundaries are stable across builds and machines.
var gear [256]uint64

func init() {
	x := uint64(0x9E3779B97F4A7C15)
	for i := range gear {
		// splitmix64
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		gear[i] = z ^ (z >> 31)
	}
}

// spreadMask returns a mask with n one-bits spread across the high 48 bits,
// as in the FastCDC paper (spread bits give better boundary distribution).
func spreadMask(n int) uint64 {
	var m uint64
	step := 48 / n
	for i := 0; i < n; i++ {
		m |= 1 << (63 - uint(i*step))
	}
	return m
}

// Cut returns the length of the first chunk in data. If data is shorter than
// MaxSize and no boundary is found, it returns len(data).
func Cut(data []byte) int {
	n := len(data)
	if n <= MinSize {
		return n
	}
	if n > MaxSize {
		n = MaxSize
	}
	normal := AvgSize
	if n < normal {
		normal = n
	}
	var h uint64
	i := MinSize
	for ; i < normal; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		h = (h << 1) + gear[data[i]]
		if h&maskL == 0 {
			return i + 1
		}
	}
	return n
}

// Split returns data divided into chunks. The chunks alias data.
func Split(data []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		c := Cut(data)
		out = append(out, data[:c])
		data = data[c:]
	}
	return out
}

// Reader yields chunks from a stream without holding the whole stream in memory.
type Reader struct {
	r   io.Reader
	buf []byte
	beg int
	end int
	eof bool
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r, buf: make([]byte, 2*MaxSize)}
}

// Next returns the next chunk, or io.EOF when the stream is exhausted. The
// returned slice is only valid until the next call.
func (c *Reader) Next() ([]byte, error) {
	if c.end-c.beg < MaxSize && !c.eof {
		copy(c.buf, c.buf[c.beg:c.end])
		c.end -= c.beg
		c.beg = 0
		for c.end < len(c.buf) && !c.eof {
			n, err := c.r.Read(c.buf[c.end:])
			c.end += n
			if err == io.EOF {
				c.eof = true
			} else if err != nil {
				return nil, err
			}
		}
	}
	if c.beg == c.end {
		return nil, io.EOF
	}
	n := Cut(c.buf[c.beg:c.end])
	out := c.buf[c.beg : c.beg+n]
	c.beg += n
	return out, nil
}
