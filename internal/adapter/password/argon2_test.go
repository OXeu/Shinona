package password

import "testing"

func TestArgon2ReadsLegacyAndConfiguredParameters(t *testing.T) {
	const value = "fixture password"
	legacy := make([]byte, 1+saltSize)
	legacy[0] = legacyVersion
	legacy = append(legacy, derive(value, legacy[1:], DefaultParameters())...)
	configured, err := New(Parameters{Time: 1, MemoryKiB: 16 * 1024, Threads: 2})
	if err != nil || !configured.Match(value, legacy) {
		t.Fatalf("configured hasher stopped accepting legacy credentials: %v", err)
	}
	newHash := configured.Hash(value)
	if plain := (Argon2id{}); !plain.Match(value, newHash) || plain.Match("another password", newHash) {
		t.Fatal("stored parameters did not control verification")
	}
	newHash[9] = 0
	if configured.Match(value, newHash) {
		t.Fatal("invalid persisted cost parameters were accepted")
	}
}
