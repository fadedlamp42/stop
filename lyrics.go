// lyrics: fetch synced lyrics from lrclib.net and parse LRC format.
//
// lrclib is a free, FOSS-friendly community lyrics database with no auth
// and a generous rate limit. it serves both plain text and timestamped LRC
// data, which lets us scroll the visible window to track the song's seek
// position in real time.
//
// fetching is cached in-memory per song key (artist|title) including
// negative results, so a long listening session sees one HTTP call per
// track. cache is process-lifetime — restarting stop re-fetches.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// LRCLine is one timestamped line of synced lyrics.
// At is offset from track start; Text is the line content (may be empty
// for intentional "blank" beats in the lyrics). Romaji is a romanization
// populated lazily for non-latin languages; empty when the line is already
// romaji-compatible or no romanizer applies. Translation is an english
// rendering of non-english Text, populated from the provider's native
// translation field when available, else filled by a translation API
// fallback; empty when text is already english or no translation is
// available.
type LRCLine struct {
	At          time.Duration
	Text        string
	Romaji      string
	Translation string
}

// Lyrics holds the result of a fetch for one track.
// when synced is non-empty the renderer can scroll line-by-line; otherwise
// it falls back to plain text. found=false means no provider had a match
// — we cache that too so we stop hammering APIs.
// source identifies which provider supplied the data; surfaced in the UI
// so a song that came from netease vs lrclib is visible at a glance.
// duration is the provider's reported track length, used to convert a
// position fraction into an absolute elapsed time. zero means unknown;
// the renderer falls back to the last LRC timestamp in that case (less
// accurate because the tail of a song is typically silent/instrumental).
// syncedIsApproximate=true means the entries in Synced were synthesized
// from Plain (no real per-line timestamps); the renderer should use
// position-fraction-based scrolling and skip the active-line highlight.
type Lyrics struct {
	QueryKey            string
	Synced              []LRCLine
	Plain               string
	Found               bool
	Source              string
	Duration            time.Duration
	SyncedIsApproximate bool
}

// lyricsCache keeps results keyed by "artist|title" for the process lifetime.
// also tracks in-flight fetches so concurrent ticks don't duplicate work.
var (
	lyricsMu       sync.Mutex
	lyricsBySong   = make(map[string]*Lyrics)
	lyricsInFlight = make(map[string]bool)
)

// parseNowPlaying splits a "ARTIST - TITLE" string from the playing script
// into its components. returns empty strings for unparseable input so the
// caller can skip fetching.
func parseNowPlaying(line string) (artist, title string) {
	line = strings.TrimSpace(line)
	if line == "" || line == "PAUSED" || line == "NONE" {
		return "", ""
	}
	idx := strings.Index(line, " - ")
	if idx < 0 {
		return "", ""
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+3:])
}

// songKey is the cache key for a track. lower-cased so "Queen - bohemian"
// and "queen - Bohemian" coalesce.
func songKey(artist, title string) string {
	return strings.ToLower(artist) + "|" + strings.ToLower(title)
}

// getCachedLyrics returns a cached lyrics result or nil if we've never
// fetched (or are still fetching) this song. callers must not mutate the
// returned pointer; it's shared across ticks.
func getCachedLyrics(artist, title string) *Lyrics {
	if artist == "" || title == "" {
		return nil
	}
	lyricsMu.Lock()
	defer lyricsMu.Unlock()
	return lyricsBySong[songKey(artist, title)]
}

// lyricsFetchInFlight reports whether a fetch is currently running for the
// given song. used by the renderer to show a "loading lyrics..." stub.
func lyricsFetchInFlight(artist, title string) bool {
	if artist == "" || title == "" {
		return false
	}
	lyricsMu.Lock()
	defer lyricsMu.Unlock()
	return lyricsInFlight[songKey(artist, title)]
}

// ensureLyricsFetch kicks off a background fetch if we don't have a cached
// result and aren't already fetching this song. returns whether a fetch
// was started (so the caller can schedule a re-render after a short
// delay). spotifyDur is forwarded to the provider chain so candidates
// can be disambiguated by runtime when title/artist don't roughly match
// (translated titles, transliterated artists).
func ensureLyricsFetch(artist, title string, spotifyDur time.Duration) bool {
	if artist == "" || title == "" {
		return false
	}
	key := songKey(artist, title)
	lyricsMu.Lock()
	if _, cached := lyricsBySong[key]; cached {
		lyricsMu.Unlock()
		return false
	}
	if lyricsInFlight[key] {
		lyricsMu.Unlock()
		return false
	}
	lyricsInFlight[key] = true
	lyricsMu.Unlock()

	go func() {
		result := fetchLyrics(artist, title, spotifyDur)
		lyricsMu.Lock()
		lyricsBySong[key] = result
		delete(lyricsInFlight, key)
		lyricsMu.Unlock()
	}()
	return true
}

// httpClient is shared so connections get reused across track fetches.
var httpClient = &http.Client{Timeout: 6 * time.Second}

const userAgent = "stop/0.1 (https://github.com/regular/stop)"

// lyricsProvider is one source of LRC/plain lyrics. providers are tried
// in order until one returns a usable result. each provider is responsible
// for its own HTTP calls, parsing, and tolerance — they return a populated
// *Lyrics on success (with Source set) or nil on miss.
//
// fetch is passed the spotify-reported track duration so providers can
// disambiguate candidates by runtime when metadata is translated /
// transliterated across services. duration is the single most reliable
// signal we have for "same song" when neither artist nor title match by
// string (e.g. spotify shows "nihosika" + english translation, netease
// has the hiragana original "にほしか" + japanese kanji title — the
// 179s runtime is what lets us connect them).
type lyricsProvider struct {
	name  string
	fetch func(artist, title string, spotifyDur time.Duration) *Lyrics
}

// lyricsProviders is the fallback chain. order matters: lrclib first
// because it's purpose-built for synced lyrics and has the cleanest data,
// netease second for its enormous indie/asian catalog (often the only
// hit for vocaloid / doujin / city-pop tracks), qq music third with
// similar coverage but a noisier search ranking.
var lyricsProviders = []lyricsProvider{
	{name: "lrclib", fetch: fetchFromLrclib},
	{name: "netease", fetch: fetchFromNetease},
	{name: "qqmusic", fetch: fetchFromQQMusic},
}

// fetchLyrics walks the provider chain and returns the first hit. always
// returns a non-nil *Lyrics so the cache layer can remember misses too
// and avoid re-hammering every provider on the next tick.
func fetchLyrics(artist, title string, spotifyDur time.Duration) *Lyrics {
	result := &Lyrics{QueryKey: songKey(artist, title)}
	for _, p := range lyricsProviders {
		hit := p.fetch(artist, title, spotifyDur)
		if hit == nil || !hit.Found {
			continue
		}
		hit.Source = p.name
		hit.QueryKey = result.QueryKey
		dropBlankLines(hit)
		// when the provider returned only unsynced plain text, synthesize
		// fake-timestamped synced entries so the same annotation + render
		// pipeline can give us romaji + translation per line. the marker
		// SyncedIsApproximate=true tells the renderer to scroll via
		// position fraction and skip active-line highlighting since the
		// timestamps aren't real.
		synthesizeSyncedFromPlain(hit)
		annotateRomaji(hit)
		annotateTranslations(hit)
		// gap markers only make sense for real per-line timestamps; skip
		// them on synthetic syncs (no actual "instrumental silences" to
		// detect when timestamps are fake).
		if !hit.SyncedIsApproximate {
			insertGapMarkers(hit)
		}
		return hit
	}
	return result
}

// durationMatchTolerance is how close two reported track lengths need to
// be to count as "same song". 3s covers normal rip-to-rip variation
// (silence trimming, fade-out cropping) without admitting genuinely
// different songs that happen to share an artist or title fragment.
const durationMatchTolerance = 3 * time.Second

// durationsMatch reports whether two durations are within the tolerance.
// returns false when either is zero so we don't false-positive on missing
// data — duration matching is opt-in and the caller falls back to other
// signals when it can't help.
func durationsMatch(a, b time.Duration) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= durationMatchTolerance
}

// annotateRomaji populates Synced[i].Romaji for any line that contains
// non-latin text. runs once per song fetch (inside the background fetch
// goroutine) so the kagome dictionary's ~1s warmup never blocks a render.
// lines that are already mostly latin get a no-op return from romanize().
func annotateRomaji(l *Lyrics) {
	for i := range l.Synced {
		l.Synced[i].Romaji = romanize(l.Synced[i].Text)
	}
}

// applyTranslationLRC parses a translation given as its own LRC string and
// merges it into l.Synced[*].Translation by matching timestamps. providers
// like netease (tlyric) and qq (trans) ship translations this way: a
// parallel LRC with the same `[mm:ss.xx]` stamps as the original. matching
// allows for ±300ms of clock drift since the translator may have rounded
// timestamps slightly differently from the original lyric file.
func applyTranslationLRC(l *Lyrics, rawLRC string) {
	if l == nil || rawLRC == "" {
		return
	}
	trans := parseLRC(rawLRC)
	if len(trans) == 0 {
		return
	}
	const tolerance = 300 * time.Millisecond
	ti := 0
	for i := range l.Synced {
		at := l.Synced[i].At
		// advance the translation cursor to the first stamp >= at-tolerance.
		for ti < len(trans) && trans[ti].At < at-tolerance {
			ti++
		}
		if ti >= len(trans) {
			break
		}
		if trans[ti].At <= at+tolerance {
			text := strings.TrimSpace(trans[ti].Text)
			if text != "" && text != l.Synced[i].Text {
				l.Synced[i].Translation = text
			}
			ti++
		}
	}
}

// synthesizeSyncedFromPlain builds fake-timestamped synced entries from
// Plain when the provider didn't supply any actual synced data. lets the
// downstream annotation + rendering pipeline give plain-text lyrics the
// same romaji + translation treatment as proper synced ones. timestamps
// are evenly distributed across the song duration so the position-
// fraction-based scroll still drifts roughly in time with playback.
//
// noop when Synced is already populated or Plain is empty.
func synthesizeSyncedFromPlain(l *Lyrics) {
	if l == nil || len(l.Synced) > 0 {
		return
	}
	plain := strings.TrimSpace(l.Plain)
	if plain == "" {
		return
	}

	var rawLines []string
	for _, raw := range strings.Split(plain, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		rawLines = append(rawLines, line)
	}
	if len(rawLines) == 0 {
		return
	}

	total := l.Duration
	if total == 0 {
		// fall back to nominal pacing — 3 seconds per line. only used for
		// the relative ordering of fake timestamps; the renderer's
		// position-fraction estimate is what actually drives scrolling.
		total = time.Duration(len(rawLines)) * 3 * time.Second
	}

	step := total / time.Duration(len(rawLines))
	out := make([]LRCLine, 0, len(rawLines))
	for i, line := range rawLines {
		out = append(out, LRCLine{
			At:   time.Duration(i) * step,
			Text: line,
		})
	}
	l.Synced = out
	l.SyncedIsApproximate = true
}

// dropBlankLines removes synced entries whose text is empty after trim.
// LRC files commonly include bare `[mm:ss.xx]` stamps as "clear display"
// markers between lyrics. our gap-marker insertion handles instrumental
// indication on its own, and the next non-blank entry's timestamp already
// communicates "previous line is no longer active", so blanks add nothing.
// dropping them BEFORE gap-marker insertion also gives the gap detector
// a clean view of inter-vocal silences.
func dropBlankLines(l *Lyrics) {
	if l == nil {
		return
	}
	out := l.Synced[:0]
	for _, line := range l.Synced {
		if strings.TrimSpace(line.Text) == "" {
			continue
		}
		out = append(out, line)
	}
	l.Synced = out
}

// -- instrumental gap markers --

// gapMarkerText is the literal text used for synthetic between-lyric
// rows. picked so the renderer can distinguish gap markers from real
// lyrics via a simple `Text == gapMarkerText` check (a struct flag
// would be cleaner but this avoids touching LRCLine consumers that
// already iterate lyrics.Synced).
const gapMarkerText = "\u266a"

// minLyricGap is the smallest silence that earns a gap marker. shorter
// gaps don't need one — the next line is on its way and an empty
// viewport for a second or two reads as breathing space rather than as
// "did the playback stall?".
const minLyricGap = 8 * time.Second

// insertGapMarkers walks the synced lines and inserts a synthetic
// instrumental marker (\u266a) at any gap longer than minLyricGap. uses
// the existing LRCLine type so the marker participates in active-line
// detection and viewport scrolling without any special-casing elsewhere.
//
// covers three gap classes: intro (before the first lyric), inter-line
// (between two lyrics), and outro (after the last lyric up to the song
// end). the outro case requires lyrics.Duration to be known — without
// a real song length we can't tell where the song actually ends, so we
// skip outro markers when duration is zero.
//
// inter-line markers fire at the GAP MIDPOINT so the previous lyric
// retains the first half of the silence as its "active" window. LRC
// timestamps only mark when a line begins; the vocal can trail on for
// several seconds after, and using a small post-line lead would force
// the marker to preempt the line while its vocal was still playing.
// midpoint gives each side a fair share of the gap and keeps sync
// matching what the listener actually hears.
func insertGapMarkers(l *Lyrics) {
	if l == nil || len(l.Synced) == 0 {
		return
	}

	out := make([]LRCLine, 0, len(l.Synced)+8)

	// intro gap: when the first lyric starts well after t=0, fill the
	// runway with a marker that's active for the whole pre-vocal stretch.
	if l.Synced[0].At >= minLyricGap {
		out = append(out, LRCLine{At: 0, Text: gapMarkerText})
	}

	for i, line := range l.Synced {
		out = append(out, line)

		var nextAt time.Duration
		if i+1 < len(l.Synced) {
			nextAt = l.Synced[i+1].At
		} else if l.Duration > 0 {
			nextAt = l.Duration
		} else {
			continue // can't compute gap to song end
		}
		gap := nextAt - line.At
		if gap < minLyricGap {
			continue
		}

		// midpoint placement: prev line stays active for the first half
		// (covers vocal trail/held notes), marker takes over for the
		// clearly-instrumental second half.
		out = append(out, LRCLine{
			At:   line.At + gap/2,
			Text: gapMarkerText,
		})
	}

	l.Synced = out
}

// annotateTranslations fills in Translation with English for every line
// whose source text isn't already English. uses google translate's
// unofficial batched endpoint — one HTTP call covers an entire song.
//
// also REPLACES non-english provider translations. netease/qq serve
// chinese users so their tlyric/trans tracks are usually in chinese,
// which isn't useful when the source line is already in chinese / when
// the viewer wants english. detecting via latinShare is good enough: a
// chinese translation is ~0% latin, an english one is ~95%+.
//
// failure is silent — the UI just won't render translation rows when
// google can't be reached.
func annotateTranslations(l *Lyrics) {
	if l == nil {
		return
	}
	var indices []int
	var texts []string
	for i := range l.Synced {
		t := l.Synced[i].Text
		if t == "" {
			continue
		}
		if latinShare(t) >= 0.85 {
			// english source — no translation needed; clear any non-english
			// provider translation that would have been redundant.
			if l.Synced[i].Translation != "" && latinShare(l.Synced[i].Translation) < 0.85 {
				l.Synced[i].Translation = ""
			}
			continue
		}
		// non-english source; only skip the google call when the provider
		// already gave us an english translation.
		if l.Synced[i].Translation != "" && latinShare(l.Synced[i].Translation) >= 0.85 {
			continue
		}
		indices = append(indices, i)
		texts = append(texts, t)
	}
	if len(texts) == 0 {
		return
	}
	results := translateBatch(texts)
	for k, idx := range indices {
		if k >= len(results) {
			break
		}
		r := strings.TrimSpace(results[k])
		if r == "" || strings.EqualFold(r, l.Synced[idx].Text) {
			continue
		}
		l.Synced[idx].Translation = r
	}
}

// -- shared provider helpers --

// httpGetJSON fetches a URL and decodes JSON into out. centralized so
// timeout + UA policy applies uniformly to every provider call.
func httpGetJSON(u string, headers map[string]string, out interface{}) error {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// artistsRoughlyMatch compares two artist names after lower-casing and
// stripping punctuation/whitespace. tolerates "Beyoncé" vs "Beyonce",
// "AC/DC" vs "ACDC", and "The Beatles" vs "Beatles".
func artistsRoughlyMatch(a, b string) bool {
	na, nb := normalizeArtist(a), normalizeArtist(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb || strings.Contains(na, nb) || strings.Contains(nb, na)
}

// titlesRoughlyMatch compares two track titles. accepts exact normalized
// equality or a strict-prefix relationship in either direction. prefix
// covers the common "extended title" cases ("Bohemian Rhapsody" vs
// "Bohemian Rhapsody (Remastered 2011)", "群青" vs "群青 - THE FIRST
// TAKE") without accepting accidental substring overlaps that would
// match unrelated songs (e.g. "Terminal" being a suffix of "my
// terminal" — different songs that happen to share a word).
func titlesRoughlyMatch(a, b string) bool {
	na, nb := normalizeTitle(a), normalizeTitle(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	return strings.HasPrefix(na, nb) || strings.HasPrefix(nb, na)
}

var (
	nonAlnum  = regexp.MustCompile(`[^\p{L}\p{N}]+`)
	parenDecor = regexp.MustCompile(`\s*\([^)]*\)\s*`)
	bracketDecor = regexp.MustCompile(`\s*\[[^\]]*\]\s*`)
)

// foldDiacritics drops combining marks so "Beyoncé" → "Beyonce" without
// losing CJK characters or other base letters. NFD splits accented chars
// into base + combining mark; we then filter the marks out.
func foldDiacritics(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func normalizeArtist(s string) string {
	s = strings.ToLower(foldDiacritics(s))
	s = strings.TrimPrefix(s, "the ")
	return nonAlnum.ReplaceAllString(s, "")
}

// normalizeTitle strips bracketed annotations like "(feat. X)" and
// "[Remastered 2011]" before normalizing, so spotify's verbose titles
// still match the lean ones providers return.
func normalizeTitle(s string) string {
	s = strings.ToLower(foldDiacritics(s))
	s = parenDecor.ReplaceAllString(s, " ")
	s = bracketDecor.ReplaceAllString(s, " ")
	return nonAlnum.ReplaceAllString(s, "")
}

// buildLyricsFromLRC packages raw LRC + (optional) plain text into a
// Lyrics struct. shared by every provider so they all parse and dedupe
// LRC the same way.
func buildLyricsFromLRC(rawLRC, rawPlain string) *Lyrics {
	synced := parseLRC(rawLRC)
	plain := stripLRCHeaders(rawPlain)
	if plain == "" && rawLRC != "" {
		// derive plain text from the LRC by stripping timestamps when no
		// dedicated plain version was supplied. lets the renderer still
		// show *something* when synced data is partial.
		plain = stripLRCHeaders(rawLRC)
	}
	if len(synced) == 0 && plain == "" {
		return nil
	}
	return &Lyrics{Synced: synced, Plain: plain, Found: true}
}

// _ unused-import sentinel so url.Values stays available if a future
// provider needs it without re-shuffling imports.
var _ = url.Values{}

// -- LRC parsing --

// lrcLineRE matches one timestamp tag like [mm:ss.xx] or [mm:ss.xxx].
// also tolerates [m:ss.xx]. capture groups: minutes, seconds, fractional.
var lrcLineRE = regexp.MustCompile(`\[(\d{1,2}):(\d{2})(?:\.(\d{1,3}))?\]`)

// parseLRC turns LRC text into a sorted slice of timestamped lines.
// header tags like [ti:...], [ar:...], [offset:...] are dropped — we only
// keep lines whose first token is a real timestamp. lines without a
// timestamp are skipped. multi-stamp lines are expanded (one entry per
// stamp).
func parseLRC(text string) []LRCLine {
	if text == "" {
		return nil
	}
	var out []LRCLine
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		matches := lrcLineRE.FindAllStringSubmatchIndex(line, -1)
		if len(matches) == 0 {
			continue
		}
		// strip all timestamp prefixes from the start to recover the text
		end := 0
		for _, m := range matches {
			if m[0] != end {
				break
			}
			end = m[1]
		}
		body := strings.TrimSpace(line[end:])
		for _, m := range matches {
			mins, _ := strconv.Atoi(line[m[2]:m[3]])
			secs, _ := strconv.Atoi(line[m[4]:m[5]])
			var frac int
			if m[6] >= 0 {
				fs := line[m[6]:m[7]]
				// normalize to milliseconds: ".5" -> 500, ".50" -> 500, ".500" -> 500
				switch len(fs) {
				case 1:
					frac, _ = strconv.Atoi(fs)
					frac *= 100
				case 2:
					frac, _ = strconv.Atoi(fs)
					frac *= 10
				case 3:
					frac, _ = strconv.Atoi(fs)
				}
			}
			at := time.Duration(mins)*time.Minute +
				time.Duration(secs)*time.Second +
				time.Duration(frac)*time.Millisecond
			out = append(out, LRCLine{At: at, Text: body})
		}
	}
	// sort by timestamp (multi-stamp expansion may have shuffled order).
	// simple insertion sort since lines are usually already in order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].At > out[j].At; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// stripLRCHeaders removes [tag:value] metadata lines from plain lyrics.
// lrclib sometimes embeds these even in the plain-text field; they're
// noise for our renderer.
func stripLRCHeaders(text string) string {
	if text == "" {
		return ""
	}
	var kept []string
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.Contains(trimmed, ":") && strings.HasSuffix(trimmed, "]") {
			// header line like [ti:..] [ar:..] [offset:..]
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// activeLineIndex returns the index of the line whose timestamp is the
// largest one <= elapsed. returns -1 when nothing has fired yet (still
// in the intro).
func activeLineIndex(lines []LRCLine, elapsed time.Duration) int {
	idx := -1
	for i, l := range lines {
		if l.At <= elapsed {
			idx = i
			continue
		}
		break
	}
	return idx
}

// formatLRCTimestamp renders a duration as mm:ss for debug/log output.
func formatLRCTimestamp(d time.Duration) string {
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%02d:%02d", mins, secs)
}
