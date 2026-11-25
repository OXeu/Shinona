package main

import "testing"

func TestWebOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin string
		valid  bool
	}{
		{"http://127.0.0.1:5174", true},
		{"http://localhost:5174", true},
		{"http://192.168.50.10:5174", true},
		{"http://[fd00::1]:5174", true},
		{"https://shinona.example.test", true},
		{"http://0.0.0.0:5174", false},
		{"http://8.8.8.8:5174", false},
		{"http://shinona.example.test", false},
		{"http://192.168.50.10:5174/settings", false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			if _, err := webOrigin(tc.origin); (err == nil) != tc.valid {
				t.Fatalf("webOrigin(%q): %v, want valid %t", tc.origin, err, tc.valid)
			}
		})
	}
}
