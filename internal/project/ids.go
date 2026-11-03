package project

import (
	"crypto/rand"
	"encoding/hex"
)

// NewChangeID returns a random, stable change ID.
func NewChangeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
