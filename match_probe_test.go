//go:build integration

package main

import (
	"fmt"
	"testing"
)

// TestProbeCurrentMatch traces what we'd find for a specific spotify song,
// showing each provider's results and the variant expansion.
// run with: go test -tags integration -run TestProbeCurrentMatch -v
func TestProbeCurrentMatch(t *testing.T) {
	artist := "PostmodernHippie"
	title := "Dreadful Everyday"
	pairs := expandSearchPairs(artist, title)
	fmt.Printf("--- variants for %s | %s ---\n", artist, title)
	for i, p := range pairs {
		fmt.Printf("  [%d] artist=%q title=%q\n", i, p.Artist, p.Title)
	}
	hit := fetchLyrics(artist, title)
	if hit == nil || !hit.Found {
		fmt.Println("MISS")
		return
	}
	fmt.Printf("HIT [%s]: synced=%d plain=%d dur=%s\n",
		hit.Source, len(hit.Synced), len(hit.Plain), hit.Duration)
	if len(hit.Synced) > 0 {
		fmt.Println("first 5 lines:")
		for i := 0; i < 5 && i < len(hit.Synced); i++ {
			fmt.Printf("  [%s] %s\n", formatLRCTimestamp(hit.Synced[i].At), hit.Synced[i].Text)
		}
	}
	if hit.Plain != "" {
		fmt.Println("plain preview (300 chars):", hit.Plain[:min(300, len(hit.Plain))])
	}
}

func min(a, b int) int { if a < b { return a }; return b }
