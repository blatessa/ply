package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/blatessa/ply/internal/chunk"
	"github.com/klauspost/compress/zstd"
)

// Hash is a SHA-256 content hash.
type Hash [32]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }
func (h Hash) Short() string  { return hex.EncodeToString(h[:6]) }
func (h Hash) IsZero() bool   { return h == Hash{} }

func (h Hash) MarshalText() ([]byte, error) { return []byte(h.String()), nil }
func (h *Hash) UnmarshalText(b []byte) error {
	p, err := ParseHash(string(b))
	*h = p
	return err
}

func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("invalid hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}

func hashFrom(b []byte) Hash {
	var h Hash
	copy(h[:], b)
	return h
}

// ErrNotFound is returned when requested content or records don't exist.
var ErrNotFound = errors.New("not found")

var (
	zenc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zdec, _ = zstd.NewReader(nil)
)

const (
	compNone = 0
	compZstd = 1
)

// EmptyBlob is the hash of zero-length content.
var EmptyBlob = Hash(sha256.Sum256(nil))

// HasChunk reports whether a chunk is stored.
func (t *Tx) HasChunk(h Hash) (bool, error) {
	var one int
	err := t.row("SELECT 1 FROM chunk WHERE hash = ?", h[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PutChunk stores a chunk if it isn't already stored and returns its hash.
func (t *Tx) PutChunk(data []byte) (Hash, error) {
	h := Hash(sha256.Sum256(data))
	ok, err := t.HasChunk(h)
	if err != nil || ok {
		return h, err
	}
	comp, stored := compNone, data
	if z := zenc.EncodeAll(data, nil); len(z) < len(data)*9/10 {
		comp, stored = compZstd, z
	}
	_, err = t.exec("INSERT INTO chunk (hash, size, comp, data) VALUES (?, ?, ?, ?)",
		h[:], len(data), comp, stored)
	return h, err
}

// Chunk returns a chunk's content.
func (t *Tx) Chunk(h Hash) ([]byte, error) {
	var comp int
	var data []byte
	err := t.row("SELECT comp, data FROM chunk WHERE hash = ?", h[:]).Scan(&comp, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("chunk %s: %w", h.Short(), ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if comp == compZstd {
		return zdec.DecodeAll(data, nil)
	}
	return data, nil
}

// BlobInfo describes a stored file's content.
type BlobInfo struct {
	Hash   Hash   `json:"hash"`
	Size   int64  `json:"size"`
	Chunks []Hash `json:"chunks"`
}

func packHashes(hs []Hash) []byte {
	b := make([]byte, 0, len(hs)*32)
	for _, h := range hs {
		b = append(b, h[:]...)
	}
	return b
}

func unpackHashes(b []byte) []Hash {
	hs := make([]Hash, len(b)/32)
	for i := range hs {
		copy(hs[i][:], b[i*32:])
	}
	return hs
}

// HasBlob reports whether a blob is stored.
func (t *Tx) HasBlob(h Hash) (bool, error) {
	var one int
	err := t.row("SELECT 1 FROM blob WHERE hash = ?", h[:]).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PutBlob stores content read from r and returns its hash.
func (t *Tx) PutBlob(r io.Reader) (BlobInfo, error) {
	var info BlobInfo
	whole := sha256.New()
	cr := chunk.NewReader(r)
	for {
		c, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return info, err
		}
		whole.Write(c)
		h, err := t.PutChunk(c)
		if err != nil {
			return info, err
		}
		info.Chunks = append(info.Chunks, h)
		info.Size += int64(len(c))
	}
	info.Hash = hashFrom(whole.Sum(nil))
	return info, t.PutBlobInfo(info)
}

// PutBlobBytes stores content and returns its hash.
func (t *Tx) PutBlobBytes(b []byte) (Hash, error) {
	info, err := t.PutBlob(bytes.NewReader(b))
	return info.Hash, err
}

// PutBlobInfo records a blob whose chunks are already stored.
func (t *Tx) PutBlobInfo(info BlobInfo) error {
	_, err := t.exec("INSERT OR IGNORE INTO blob (hash, size, chunks) VALUES (?, ?, ?)",
		info.Hash[:], info.Size, packHashes(info.Chunks))
	return err
}

// Blob returns a blob's description.
func (t *Tx) Blob(h Hash) (BlobInfo, error) {
	info := BlobInfo{Hash: h}
	var packed []byte
	err := t.row("SELECT size, chunks FROM blob WHERE hash = ?", h[:]).Scan(&info.Size, &packed)
	if errors.Is(err, sql.ErrNoRows) {
		return info, fmt.Errorf("content %s: %w", h.Short(), ErrNotFound)
	}
	info.Chunks = unpackHashes(packed)
	return info, err
}

// WriteBlob writes a blob's content to w.
func (t *Tx) WriteBlob(w io.Writer, h Hash) error {
	info, err := t.Blob(h)
	if err != nil {
		return err
	}
	for _, c := range info.Chunks {
		data, err := t.Chunk(c)
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// ReadBlob returns a blob's full content.
func (t *Tx) ReadBlob(h Hash) ([]byte, error) {
	var buf bytes.Buffer
	err := t.WriteBlob(&buf, h)
	return buf.Bytes(), err
}
