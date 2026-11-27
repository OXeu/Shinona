package mcpconfig

import (
	"strings"
	"unicode/utf8"
)

const MaxServers = 16

func ValidName(name string) bool {
	const maxNameBytes = 80
	return len(name) <= maxNameBytes && strings.TrimSpace(name) != "" && utf8.ValidString(name) &&
		!strings.ContainsAny(name, "\x00\r\n")
}

func ValidSelection(names []string) bool {
	if len(names) > MaxServers {
		return false
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !ValidName(name) || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}
