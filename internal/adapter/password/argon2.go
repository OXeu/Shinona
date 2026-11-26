// Package password stores versioned Argon2id password hashes.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"golang.org/x/crypto/argon2"
)

const (
	legacyVersion = 1
	version       = 2
	headerSize    = 10
	saltSize      = 16
	keySize       = 32
)

type Parameters struct {
	Time, MemoryKiB uint32
	Threads         uint8
}

func DefaultParameters() Parameters { return Parameters{Time: 3, MemoryKiB: 64 * 1024, Threads: 1} }

func (p Parameters) valid() bool {
	return p.Time >= 1 && p.Time <= 10 && p.MemoryKiB >= 16*1024 && p.MemoryKiB <= 256*1024 &&
		p.Threads >= 1 && p.Threads <= 8
}

type Argon2id struct{ parameters Parameters }

func New(parameters Parameters) (Argon2id, error) {
	if !parameters.valid() {
		return Argon2id{}, errors.New("Argon2id requires time 1–10, memory 16384–262144 KiB, and threads 1–8")
	}
	return Argon2id{parameters: parameters}, nil
}

func (a Argon2id) Hash(password string) []byte {
	params := a.parameters
	if params == (Parameters{}) {
		params = DefaultParameters()
	}
	hash := make([]byte, headerSize+saltSize, headerSize+saltSize+keySize)
	hash[0] = version
	binary.BigEndian.PutUint32(hash[1:5], params.Time)
	binary.BigEndian.PutUint32(hash[5:9], params.MemoryKiB)
	hash[9] = params.Threads
	salt := hash[headerSize:]
	_, _ = rand.Read(salt)
	return append(hash, derive(password, salt, params)...)
}

func (Argon2id) Match(password string, hash []byte) bool {
	var header int
	var params Parameters
	switch {
	case len(hash) == 1+saltSize+keySize && hash[0] == legacyVersion:
		header, params = 1, DefaultParameters()
	case len(hash) == headerSize+saltSize+keySize && hash[0] == version:
		header = headerSize
		params = Parameters{
			Time: binary.BigEndian.Uint32(hash[1:5]), MemoryKiB: binary.BigEndian.Uint32(hash[5:9]), Threads: hash[9],
		}
		if !params.valid() {
			return false
		}
	default:
		return false
	}
	offset := header + saltSize
	return subtle.ConstantTimeCompare(derive(password, hash[header:offset], params), hash[offset:]) == 1
}

func derive(password string, salt []byte, params Parameters) []byte {
	return argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Threads, keySize)
}
