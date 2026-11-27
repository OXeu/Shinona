package pigo

import (
	"math"

	"github.com/sky-valley/pi/ai"
)

func contextTokens(usage ai.Usage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.Input + usage.CacheRead + usage.CacheWrite + usage.Output
}

func clampTokenCount(tokens int) int32 {
	return int32(min(math.MaxInt32, max(0, tokens)))
}

func addTokenCount(total int32, tokens int) int32 {
	return total + min(math.MaxInt32-total, clampTokenCount(tokens))
}
