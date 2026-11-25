package pigo

import "github.com/guanshan/pi-go/packages/ai"

func contextTokens(usage ai.Usage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.CacheRead + usage.CacheWrite + usage.Output
}
