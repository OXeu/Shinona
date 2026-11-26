package mcpconfig

const MaxServers = 16

func ValidName(name string) bool {
	const maxNameBytes = 80
	return name != "" && len(name) <= maxNameBytes
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
