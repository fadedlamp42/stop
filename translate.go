// translate: english translation fallback for lyric lines via google's
// unofficial gtx endpoint.
//
// the gtx client is the same channel that google translate's web/mobile
// frontends use for casual queries. it requires no key, accepts auto
// source-language detection, and returns plain JSON. rate limits do
// apply at the level of "many calls per second from one IP", which is
// not a regime we ever approach: one batched call per song.
//
// when providers (netease, qq) ship native translations alongside their
// lyrics, those are used and this module is not called for those lines.
// the API call only fires for the gap-filling case (typically lrclib
// hits where no translation is included).

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// -- title translation cache --
//
// the now-playing strip shows "ARTIST - TITLE" raw; when either field
// contains non-latin script we kick off a background google-translate
// call and (when it returns) render the english version on a second line
// below. cached per (artist|title) string for the process lifetime so
// switching between known songs costs no API round-trips.
//
// the entry's Done flag distinguishes "translation finished, here's
// nothing" (already english, or google returned empty) from "not yet
// fetched, show nothing yet" — without a Done flag the renderer can't
// tell which case it's in.

type titleTransEntry struct {
	Translation string
	Done        bool
}

var (
	titleTransMu       sync.Mutex
	titleTransByKey    = make(map[string]titleTransEntry)
	titleTransInFlight = make(map[string]bool)
)

// getCachedTitleTranslation returns the english translation cached for
// (artist, title). returns "" when the translation is still pending OR
// when no translation is needed (latin source). callers should not
// distinguish between these — both render as "no extra line".
func getCachedTitleTranslation(artist, title string) string {
	if artist == "" && title == "" {
		return ""
	}
	titleTransMu.Lock()
	defer titleTransMu.Unlock()
	return titleTransByKey[artist+"|"+title].Translation
}

// ensureTitleTranslation kicks off a background translation when the
// title or artist contains non-latin text and we haven't already
// translated this pair. caches both positive and negative results so
// we don't re-hit google every render.
func ensureTitleTranslation(artist, title string) {
	if artist == "" && title == "" {
		return
	}
	// translate artist + " - " + title as one string so google sees them
	// in context (proper-noun person names romanize correctly only with
	// context — translating "式浦躁吾" alone gives mandarin pinyin
	// "Shipu Zaowu" instead of japanese "Shikiura Zago"). the literal
	// " - " separator survives the translation pipeline so we can render
	// the result with the dash preserved between fields.
	combined := strings.TrimSpace(artist + " - " + title)
	if combined == "" {
		return
	}
	if latinShare(combined) >= 0.85 {
		return
	}

	key := artist + "|" + title
	titleTransMu.Lock()
	if entry, ok := titleTransByKey[key]; ok && entry.Done {
		titleTransMu.Unlock()
		return
	}
	if titleTransInFlight[key] {
		titleTransMu.Unlock()
		return
	}
	titleTransInFlight[key] = true
	titleTransMu.Unlock()

	go func() {
		result := translateBatch([]string{combined})

		titleTransMu.Lock()
		defer titleTransMu.Unlock()
		delete(titleTransInFlight, key)
		entry := titleTransEntry{Done: true}
		if len(result) > 0 {
			t := strings.TrimSpace(result[0])
			if t != "" && !strings.EqualFold(t, combined) {
				entry.Translation = t
			}
		}
		titleTransByKey[key] = entry
	}()
}

// translateBatch translates a slice of lines from auto-detected source to
// english in a single HTTP call. preserves order and length: result[i]
// is the english translation of inputs[i], or "" if translation failed
// for that line. failures are silent — the UI just skips empty results.
//
// implementation joins inputs with a unique delimiter, sends one request,
// then splits the response back. this keeps us at one round-trip per song
// instead of N (number of non-english lyric lines).
func translateBatch(inputs []string) []string {
	out := make([]string, len(inputs))
	if len(inputs) == 0 {
		return out
	}

	// delimiter "@@@" is extremely unlikely in natural lyrics and survives
	// google's translation pipeline as a literal pass-through. google often
	// inserts a space on either side ("foo @@@ bar"), so split with a
	// regex that swallows surrounding whitespace.
	const delim = "@@@"
	joined := strings.Join(inputs, delim)

	// google translate's web/mobile API: gtx client, target english
	// (tl=en), retrieve translation (dt=t). source language is detected
	// from the joined input — defaulting to sl=auto sounds fine in
	// theory but in practice google misdetects mostly-latin batches with
	// a few non-latin words, and worse, refuses to translate romaji even
	// when sl=ja is passed. our detection biases toward the dominant
	// non-latin script (kana → ja, hangul → ko, han-only → zh-CN) and
	// falls back to auto when the input is mostly latin.
	sl := detectSourceLanguage(joined)
	u := "https://translate.googleapis.com/translate_a/single?" + url.Values{
		"client": {"gtx"},
		"sl":     {sl},
		"tl":     {"en"},
		"dt":     {"t"},
		"q":      {joined},
	}.Encode()

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; stop/0.1)")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out
	}

	// gtx response shape: [[[ "translated", "original", null, null, ... ],
	//                       [ "translated2", "original2", ... ], ...],
	//                       null, "ja", ...]
	// each outer item in [0] is a sentence-level translation block;
	// google sometimes breaks our single joined string across multiple
	// blocks for long inputs. concatenating all translated halves and
	// then splitting by our delimiter recovers the per-line layout.
	var decoded []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return out
	}
	if len(decoded) == 0 {
		return out
	}
	sentences, ok := decoded[0].([]interface{})
	if !ok {
		return out
	}
	var b strings.Builder
	for _, s := range sentences {
		row, ok := s.([]interface{})
		if !ok || len(row) == 0 {
			continue
		}
		piece, _ := row[0].(string)
		b.WriteString(piece)
	}
	combined := b.String()
	parts := delimSplitter.Split(combined, -1)
	for i := 0; i < len(out) && i < len(parts); i++ {
		out[i] = strings.TrimSpace(parts[i])
	}
	return out
}

// delimSplitter tolerates the whitespace google sometimes inserts around
// our "@@@" sentinel ("foo @@@ bar", "foo@@@bar", "foo  @@@  bar" are
// all valid post-translation forms).
var delimSplitter = regexp.MustCompile(`\s*@@@\s*`)

// detectSourceLanguage picks the most reliable `sl=` value for a chunk
// of text. presence of kana → "ja" (even alongside han chars, since kana
// is unambiguously japanese). hangul → "ko". han-only → "zh-CN".
// otherwise "auto" — let google guess, since the input might be a
// european language or a mix without a dominant non-latin script.
//
// passing an explicit sl whenever we can defends against the most
// common no-op pattern: google misdetecting a mostly-latin batch with a
// few non-latin words as english and returning the input unchanged.
func detectSourceLanguage(s string) string {
	var kana, hangul, han int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			kana++
		case unicode.Is(unicode.Hangul, r):
			hangul++
		case unicode.Is(unicode.Han, r):
			han++
		}
	}
	if kana > 0 {
		return "ja"
	}
	if hangul > 0 {
		return "ko"
	}
	if han > 0 {
		return "zh-CN"
	}
	return "auto"
}
