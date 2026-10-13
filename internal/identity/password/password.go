// Package password hashes and verifies passwords with Argon2id (ADR-04, NN rules in brief 6).
// Hashes use the standard PHC string format so parameters can be raised later without a schema change.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Parameters (S-24): 64 MiB, 2 passes, 2 lanes, 16-byte salt, 32-byte key. Login attempts are rate limited,
// so this favours memory hardness over speed. Re-check on the real Render instance size before go-live.
const (
	memoryKiB uint32 = 64 * 1024
	timeCost  uint32 = 2
	threads   uint8  = 2
	saltLen          = 16
	keyLen    uint32 = 32

	MinLength = 12
	MaxLength = 128
)

var ErrMismatch = errors.New("password does not match")

// Hash returns a PHC-format Argon2id hash with a fresh random salt.
func Hash(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(plain), salt, timeCost, memoryKiB, threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, memoryKiB, timeCost, threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

type params struct {
	m, t uint32
	p    uint8
	salt []byte
	key  []byte
}

func parse(encoded string) (params, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return params{}, errors.New("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return params{}, errors.New("unsupported argon2 version")
	}
	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.m, &p.t, &p.p); err != nil {
		return params{}, errors.New("bad argon2 parameters")
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return params{}, errors.New("bad salt")
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < 16 {
		return params{}, errors.New("bad key")
	}
	if p.m == 0 || p.t == 0 || p.p == 0 || p.m > 1<<20 {
		return params{}, errors.New("unreasonable argon2 parameters")
	}
	return p, nil
}

// Verify checks plain against an encoded hash in constant time. It returns nil on a match.
func Verify(plain, encoded string) error {
	p, err := parse(encoded)
	if err != nil {
		return err
	}
	got := argon2.IDKey([]byte(plain), p.salt, p.t, p.m, p.p, uint32(len(p.key)))
	if subtle.ConstantTimeCompare(got, p.key) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether the stored hash uses weaker parameters than the current ones.
func NeedsRehash(encoded string) bool {
	p, err := parse(encoded)
	if err != nil {
		return true
	}
	return p.m < memoryKiB || p.t < timeCost || p.p < threads
}

// Policy checks a new password. It is deliberately simple: length matters most (S-25).
func Policy(plain, username string, banned ...string) error {
	n := len([]rune(plain))
	switch {
	case n < MinLength:
		return fmt.Errorf("password must be at least %d characters", MinLength)
	case n > MaxLength:
		return fmt.Errorf("password must be at most %d characters", MaxLength)
	}
	lower := strings.ToLower(plain)
	if username != "" && strings.Contains(lower, strings.ToLower(username)) {
		return errors.New("password must not contain the username")
	}
	for _, b := range banned {
		if plain == b {
			return errors.New("this password is not allowed")
		}
	}
	if strings.Count(plain, string([]rune(plain)[0])) == n {
		return errors.New("password must not be one repeated character")
	}
	return nil
}
