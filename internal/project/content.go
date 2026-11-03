package project

import (
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/store"
)

// MissingChunks returns the chunks of the given blobs that aren't stored.
// Blobs already stored are skipped entirely.
func MissingChunks(tx *store.Tx, blobs []store.BlobInfo) ([]store.Hash, error) {
	var out []store.Hash
	seen := map[store.Hash]bool{}
	for _, b := range blobs {
		ok, err := tx.HasBlob(b.Hash)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		for _, c := range b.Chunks {
			if seen[c] {
				continue
			}
			seen[c] = true
			ok, err := tx.HasChunk(c)
			if err != nil {
				return nil, err
			}
			if !ok {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// SnapshotBlobs describes the blobs needed to have the want snapshots,
// leaving out files whose content appears in the have snapshots. Unknown
// have snapshots are ignored.
func SnapshotBlobs(tx *store.Tx, want, have []store.Hash) ([]store.BlobInfo, error) {
	known := map[store.Hash]bool{}
	for _, h := range have {
		s, err := tx.Snapshot(h)
		if err != nil {
			continue
		}
		known[h] = true
		for _, e := range s.Entries {
			known[e.Hash] = true
		}
	}
	var out []store.BlobInfo
	add := func(h store.Hash) error {
		if known[h] {
			return nil
		}
		known[h] = true
		info, err := tx.Blob(h)
		if err != nil {
			return err
		}
		out = append(out, info)
		return nil
	}
	for _, w := range want {
		if known[w] {
			continue
		}
		s, err := tx.Snapshot(w)
		if err != nil {
			return nil, err
		}
		if err := add(w); err != nil {
			return nil, err
		}
		for _, e := range s.Entries {
			if err := add(e.Hash); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// StoreBlobs records blob descriptions whose chunks are all stored.
func StoreBlobs(tx *store.Tx, blobs []store.BlobInfo) error {
	for _, b := range blobs {
		for _, c := range b.Chunks {
			ok, err := tx.HasChunk(c)
			if err != nil {
				return err
			}
			if !ok {
				return errMissingChunk(b.Hash)
			}
		}
		if err := tx.PutBlobInfo(b); err != nil {
			return err
		}
	}
	return nil
}

func errMissingChunk(blob store.Hash) error {
	return plyerr.New(plyerr.Invalid, "ply share", "content %s is incomplete; a chunk is missing", blob.Short())
}
