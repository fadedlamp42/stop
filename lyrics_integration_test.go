//go:build integration

package main

import (
	"fmt"
	"testing"
)

// TestLyricsLiveProviders hits the real lyrics providers and prints which
// ones return data for a curated set of tracks. opt-in via:
//   go test -tags integration -run TestLyricsLiveProviders -v ./...
func TestLyricsLiveProviders(t *testing.T) {
	cases := []struct{ artist, title string }{
		{"Queen", "Bohemian Rhapsody"},        // canonical western — lrclib should win
		{"YOASOBI", "群青"},                     // popular j-pop CJK title — should hit on kanji + romaji variants
		{"Aoris", "Namelessness"},             // indie / aliased artist — netease finds it as 青栗鼠 / ななしのやまい
		{"Hatsune Miku", "World is Mine"},     // vocaloid — qq/netease territory
		{"ヨアソビ", "群青"},                       // pure-CJK metadata — exercises romaji variant
		{"This Band Does Not Exist", "Track"}, // intentional miss
	}
	for _, c := range cases {
		pairs := expandSearchPairs(c.artist, c.title)
		fmt.Printf("\n--- %s | %s (variants: %d) ---\n", c.artist, c.title, len(pairs))
		for i, p := range pairs {
			fmt.Printf("    [%d] %s | %s\n", i, p.Artist, p.Title)
		}
		hit := fetchLyrics(c.artist, c.title)
		if hit == nil || !hit.Found {
			fmt.Printf("[MISS] %s - %s\n", c.artist, c.title)
			continue
		}
		var tailLRC string
		if len(hit.Synced) > 0 {
			tailLRC = formatLRCTimestamp(hit.Synced[len(hit.Synced)-1].At)
		}
		markers := 0
		for _, ln := range hit.Synced {
			if ln.Text == gapMarkerText {
				markers++
			}
		}
		fmt.Printf("[%-7s] %s - %s   synced=%d (gap-markers=%d) plain=%d dur=%s last_lrc=%s\n",
			hit.Source, c.artist, c.title, len(hit.Synced), markers, len(hit.Plain),
			hit.Duration.Round(1e9), tailLRC)
		// preview the first few non-trivial lines so we can eyeball
		// romanization + translation quality at a glance.
		previewed := 0
		for _, ln := range hit.Synced {
			if previewed >= 3 || (ln.Romaji == "" && ln.Translation == "") {
				continue
			}
			fmt.Printf("    %s\n", ln.Text)
			if ln.Romaji != "" {
				fmt.Printf("       romaji: %s\n", ln.Romaji)
			}
			if ln.Translation != "" {
				fmt.Printf("       trans : %s\n", ln.Translation)
			}
			previewed++
		}
	}
}
