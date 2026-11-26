package responsecache

import "testing"

func TestCacheCapacityAndReplacement(t *testing.T) {
	disabled := New[string, string](0, 8)
	disabled.Put("disabled", "value", 5)
	if _, ok := disabled.Get("disabled"); ok {
		t.Fatal("a disabled cache retained an entry")
	}
	cache := New[string, string](2, 8)
	cache.Put("invalid", "value", -1)
	cache.Put("first", "old", 6)
	cache.Put("first", "new", 4)
	cache.Put("second", "second", 4)
	if value, ok := cache.Get("first"); !ok || value != "new" {
		t.Fatalf("cache lost a replacement: %q", value)
	}
	cache.Put("third", "third", 4)
	if _, ok := cache.Get("second"); ok {
		t.Fatal("least recently used entry survived eviction")
	}
	if _, ok := cache.Get("invalid"); ok || cache.bytes != 8 {
		t.Fatalf("invalid entry or incorrect size: %d", cache.bytes)
	}
}
