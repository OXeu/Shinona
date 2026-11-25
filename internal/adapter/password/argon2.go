// Package password stores versioned Argon2id password hashes.
package password

import (
	"crypto/rand"
	"crypto/subtle"

	"golang.org/x/crypto/argon2"
)

const (
	version  = 1
	saltSize = 16
	keySize  = 32
)

type Argon2id struct{}

func (Argon2id) Hash(password string) []byte {
	hash := make([]byte, 1+saltSize, 1+saltSize+keySize)
	hash[0] = version
	salt := hash[1:]
	_, _ = rand.Read(salt)
	return append(hash, derive(password, salt)...)
}

func (Argon2id) Match(password string, hash []byte) bool {
	if len(hash) != 1+saltSize+keySize || hash[0] != version {
		return false
	}
	offset := 1 + saltSize
	return subtle.ConstantTimeCompare(derive(password, hash[1:offset]), hash[offset:]) == 1
}

func derive(password string, salt []byte) []byte {
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, keySize)
}
