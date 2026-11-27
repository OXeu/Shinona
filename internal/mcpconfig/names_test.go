package mcpconfig

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelectionLimits(t *testing.T) {
	names := make([]string, 0, MaxServers)
	for i := range MaxServers {
		names = append(names, fmt.Sprintf("server-%d", i))
	}
	last := len(names) - 1
	names[last] = strings.Repeat("é", 40)
	if !ValidSelection(names) {
		t.Fatal("rejected the maximum number of servers and an 80-byte name")
	}
	if ValidSelection(append(names, "extra")) {
		t.Fatal("accepted a selection over the server limit")
	}
	for _, invalid := range []string{names[last] + "x", names[0], "", " ", "line\nfeed", "carriage\rreturn", "null\x00byte"} {
		names[last] = invalid
		if ValidSelection(names) {
			t.Errorf("accepted invalid server name %q", invalid)
		}
	}
}
