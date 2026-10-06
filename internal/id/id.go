// Package id generates identifiers and opaque tokens.
package id

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
)

// NewID returns a UUIDv7 string for stable, sortable identifiers.
func NewID() string {
	u, _ := uuid.NewV7()
	return u.String()
}

// NewToken returns a fresh opaque bearer token string.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken derives the stored form of a bearer token (SHA-256, never raw).
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
