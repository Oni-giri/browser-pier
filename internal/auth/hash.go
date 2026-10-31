// Package auth handles the admin login, agent tokens and the handoff PIN,
// all hashed at rest, plus rate limits. Nothing here logs a secret.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for the admin password and the PIN: OWASP's
// recommended minimum, which costs about 20 MiB and a few tens of
// milliseconds, fine on a small home server.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	argonSaltLen = 16
)

var errBadHash = errors.New("malformed password hash")

// HashSecret hashes a low-entropy secret (a password or PIN) with
// argon2id, in the PHC string format.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifySecret reports whether secret matches a hash from HashSecret. It
// reads the parameters from the hash, so old hashes keep working if the
// defaults change.
func VerifySecret(encoded, secret string) bool {
	m, t, p, salt, key, err := parseHash(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, t, m, p, uint32(len(key))) //nolint:gosec // len(key) is 32 from parseHash
	return subtle.ConstantTimeCompare(got, key) == 1
}

func parseHash(encoded string) (m, t uint32, p uint8, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return 0, 0, 0, nil, nil, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return 0, 0, 0, nil, nil, errBadHash
	}
	// Refuse parameters far outside what HashSecret writes, so a tampered
	// row can't make verification eat the machine.
	if m == 0 || m > 256*1024 || t == 0 || t > 10 || p == 0 || p > 8 {
		return 0, 0, 0, nil, nil, errBadHash
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return 0, 0, 0, nil, nil, errBadHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) != argonKeyLen {
		return 0, 0, 0, nil, nil, errBadHash
	}
	return m, t, p, salt, key, nil
}
