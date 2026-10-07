// Package pwd hashes and verifies passwords with Argon2id.
package pwd

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const (
	memoryKiB = 64 * 1024
	threads   = 4
	saltLen   = 16
	keyLen    = 32
	version   = 1
)

// BCryptPrefix marks a Rocket.Chat/Meteor legacy hash copied verbatim by the
// migration: bcrypt over the SHA-256 *hex digest string* of the password.
// Verified once, then transparently re-hashed to argon2id on login.
const BCryptPrefix = "bcrypt$"

// Hash returns a PHC-style argon2id string:
// argon2id$<version>,<memoryKiB>,<threads>,<keyLen>$<salt>$<key>
func Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("salt rand: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, version, memoryKiB, threads, keyLen)
	saltB64 := base64.RawStdEncoding.EncodeToString(salt)
	keyB64 := base64.RawStdEncoding.EncodeToString(key)
	return fmt.Sprintf("argon2id$%d,%d,%d,%d$%s$%s", version, memoryKiB, threads, keyLen, saltB64, keyB64), nil
}

// Verify reports whether the password matches the stored hash. Crafted hashes
// with absurd work factors are rejected so a hostile database cannot turn
// login into a CPU DoS. Legacy bcrypt$ hashes (migration import) verify
// against bcrypt(SHA-256(pw) as hex).
func Verify(stored, password string) (bool, error) {
	if rest, ok := strings.CutPrefix(stored, BCryptPrefix); ok {
		sum := sha256.Sum256([]byte(password))
		err := bcrypt.CompareHashAndPassword([]byte(rest), []byte(hex.EncodeToString(sum[:])))
		return err == nil, nil // ponytail: bcrypt mismatch error adds nothing here; caller checks ok only
	}
	rest, ok := strings.CutPrefix(stored, "argon2id$")
	if !ok {
		return false, errors.New("malformed password hash")
	}
	head, tail, ok := strings.Cut(rest, "$")
	if !ok {
		return false, errors.New("malformed password hash")
	}
	saltB64, keyB64, ok := strings.Cut(tail, "$")
	if !ok {
		return false, errors.New("malformed password hash")
	}
	params := strings.Split(head, ",")
	if len(params) != 4 || params[0] != "1" {
		return false, errors.New("malformed argon2 parameters")
	}
	vals := make([]int, 3)
	for i, p := range params[1:] {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 {
			return false, errors.New("malformed argon2 parameters")
		}
		vals[i] = n
	}
	mem, threads, keyLen2 := vals[0], vals[1], vals[2]
	if mem > 256*1024 || threads > 16 || keyLen2 > 128 {
		return false, errors.New("argon2 parameters outside allowed bounds")
	}
	salt, err := base64.RawStdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) < 8 {
		return false, errors.New("malformed salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(keyB64)
	if err != nil || len(want) < 16 {
		return false, errors.New("malformed key")
	}
	got := argon2.IDKey([]byte(password), salt, version, uint32(mem), uint8(threads), uint32(keyLen2))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
