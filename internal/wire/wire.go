// Package wire is the format for moving content between a machine and the
// server: request types, and a framed binary pack for chunks so large files
// aren't inflated by JSON encoding.
package wire

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
)

// MissingRequest asks which chunks of these blobs the server lacks.
type MissingRequest struct {
	Blobs []store.BlobInfo `json:"blobs"`
}

// MissingResponse lists chunks to upload.
type MissingResponse struct {
	Chunks []store.Hash `json:"chunks"`
}

// ShareMeta is the first frame of a share upload.
type ShareMeta struct {
	project.ShareRequest
	Blobs []store.BlobInfo `json:"blobs"`
}

// FetchRequest asks for the blob descriptions needed to have the Want
// snapshots, given the client already has the Have snapshots.
type FetchRequest struct {
	Want []store.Hash `json:"want"`
	Have []store.Hash `json:"have"`
}

// FetchResponse describes blobs the client may need.
type FetchResponse struct {
	Blobs []store.BlobInfo `json:"blobs"`
}

// ChunksRequest asks for chunk contents; the reply is a pack.
type ChunksRequest struct {
	Chunks []store.Hash `json:"chunks"`
}

// Frame types.
const (
	frameJSON  = 'J'
	frameChunk = 'C'
	frameEnd   = 'E'
)

// MaxFrame bounds a frame's size: chunks are at most 128 KiB; metadata can
// be larger for big trees.
const MaxFrame = 256 << 20

// PackWriter writes frames.
type PackWriter struct {
	w *bufio.Writer
}

func NewPackWriter(w io.Writer) *PackWriter { return &PackWriter{w: bufio.NewWriterSize(w, 256<<10)} }

func (p *PackWriter) frame(kind byte, b []byte) error {
	var hdr [1 + binary.MaxVarintLen64]byte
	hdr[0] = kind
	n := binary.PutUvarint(hdr[1:], uint64(len(b)))
	if _, err := p.w.Write(hdr[:1+n]); err != nil {
		return err
	}
	_, err := p.w.Write(b)
	return err
}

// JSON writes a metadata frame.
func (p *PackWriter) JSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.frame(frameJSON, b)
}

// Chunk writes a chunk frame.
func (p *PackWriter) Chunk(data []byte) error { return p.frame(frameChunk, data) }

// Close ends the pack and flushes it.
func (p *PackWriter) Close() error {
	if err := p.frame(frameEnd, nil); err != nil {
		return err
	}
	return p.w.Flush()
}

// PackReader reads frames.
type PackReader struct {
	r *bufio.Reader
}

func NewPackReader(r io.Reader) *PackReader { return &PackReader{r: bufio.NewReaderSize(r, 256<<10)} }

// ErrEnd marks the end of a pack.
var ErrEnd = errors.New("end of pack")

func (p *PackReader) next() (byte, []byte, error) {
	kind, err := p.r.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("truncated pack: %w", err)
	}
	n, err := binary.ReadUvarint(p.r)
	if err != nil {
		return 0, nil, err
	}
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("pack frame too large (%d bytes)", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(p.r, b); err != nil {
		return 0, nil, err
	}
	if kind == frameEnd {
		return kind, nil, ErrEnd
	}
	return kind, b, nil
}

// JSON reads a metadata frame into v.
func (p *PackReader) JSON(v any) error {
	kind, b, err := p.next()
	if err != nil {
		return err
	}
	if kind != frameJSON {
		return fmt.Errorf("expected metadata frame, got %q", kind)
	}
	return json.Unmarshal(b, v)
}

// Chunk reads the next chunk, returning its hash, or ErrEnd.
func (p *PackReader) Chunk() (store.Hash, []byte, error) {
	kind, b, err := p.next()
	if err != nil {
		return store.Hash{}, nil, err
	}
	if kind != frameChunk {
		return store.Hash{}, nil, fmt.Errorf("expected chunk frame, got %q", kind)
	}
	return store.Hash(sha256.Sum256(b)), b, nil
}
