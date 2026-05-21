// lyrics_providers: individual fetchers for each upstream lyrics source.
//
// each function takes the spotify-reported artist + title, hits the
// provider's public API, and returns either a populated *Lyrics (with
// Found=true) or nil to signal "no usable hit, try the next provider".
// none of these require API keys — they all rely on public endpoints
// that mobile clients and web frontends use.
//
// keeping providers in their own file means lyrics.go stays focused on
// types, caching, and the chain controller, while each source's quirks
// (encoding, response shape, ranking tricks) live with the call that
// triggers them.

package main

import (
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// titleSpecificEnough gates the title-only fallback paths. very short or
// very common titles ("Track", "Home", "Two") return false because a
// title-only match is more likely a false positive than a real hit for
// those queries. >=7 runes catches most generic single-word titles while
// allowing legitimate ones like "Bohemian", "Imagine", "Yesterday".
// CJK titles get a lower threshold because every kanji carries more
// information per char than a latin letter.
func titleSpecificEnough(title string) bool {
	n := utf8.RuneCountInString(title)
	if n >= 7 {
		return true
	}
	// CJK: 3+ chars worth ranking on
	for _, r := range title {
		if r > 0x3000 {
			return n >= 3
		}
	}
	return false
}

// -- lrclib (https://lrclib.net) --
//
// purpose-built community LRC database. /api/get expects exact metadata;
// /api/search is fuzzy. excellent for western pop / rock / indie; weak
// for asian / vocaloid / doujin catalogs.

type lrclibResult struct {
	ID           int     `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

func fetchFromLrclib(artist, title string) *Lyrics {
	// for each (artist, title) variant — original first, then transliterated
	// forms — try the full query chain. order matters: variants come in
	// rough confidence order so the most-likely-correct one is consulted
	// before the speculative ones.
	for pi, pair := range expandSearchPairs(artist, title) {
		// exact match is only meaningful for the canonical pair (original
		// metadata as reported by spotify); transliterations are by
		// definition fuzzy, so skip the /api/get fast path for them.
		if pi == 0 {
			var exact lrclibResult
			exactURL := "https://lrclib.net/api/get?" + url.Values{
				"artist_name": {pair.Artist},
				"track_name":  {pair.Title},
			}.Encode()
			if err := httpGetJSON(exactURL, nil, &exact); err == nil {
				if hit := buildLyricsFromLRC(exact.SyncedLyrics, exact.PlainLyrics); hit != nil {
					hit.Duration = time.Duration(exact.Duration * float64(time.Second))
					return hit
				}
			}
		}

		// fuzzy chain: structured > free-form > title-only (gated).
		queries := []url.Values{
			{"track_name": {pair.Title}, "artist_name": {pair.Artist}},
			{"q": {pair.Artist + " " + pair.Title}},
		}
		if titleSpecificEnough(pair.Title) {
			queries = append(queries,
				url.Values{"track_name": {pair.Title}},
				url.Values{"q": {pair.Title}},
			)
		}

		for _, q := range queries {
			var hits []lrclibResult
			if err := httpGetJSON("https://lrclib.net/api/search?"+q.Encode(), nil, &hits); err != nil {
				continue
			}
			// strict acceptance: require artist OR title to roughly match.
			// the previous "accept first hit with lyrics" fallback was
			// a false-positive farm — when a track had no lyrics on
			// lrclib, we'd return some unrelated song's lyrics just
			// because they were the first non-empty result.
			for _, h := range hits {
				artistOK := artistsRoughlyMatch(h.ArtistName, pair.Artist) ||
					artistsRoughlyMatch(h.ArtistName, artist)
				titleOK := titlesRoughlyMatch(h.TrackName, pair.Title) ||
					titlesRoughlyMatch(h.TrackName, title)
				if !artistOK && !titleOK {
					continue
				}
				if hit := buildLyricsFromLRC(h.SyncedLyrics, h.PlainLyrics); hit != nil {
					hit.Duration = time.Duration(h.Duration * float64(time.Second))
					return hit
				}
			}
		}
	}
	return nil
}

// -- netease (music.163.com) --
//
// chinese streaming giant; its catalog covers an enormous amount of
// j-pop, vocaloid, doujin, and indie work that western databases miss
// entirely. the public `/api/search/get/` + `/api/song/lyric` endpoints
// don't require auth or encryption (the newer `/api/search/pc` and weapi
// paths do — we deliberately stick to the legacy endpoints).
//
// matching is forgiving: netease often files songs under their original
// japanese name with a localized artist, so when artist match fails we
// fall back to title-only matching.

type neteaseSearchResp struct {
	Result struct {
		Songs []neteaseSong `json:"songs"`
	} `json:"result"`
}

type neteaseSong struct {
	ID       int             `json:"id"`
	Name     string          `json:"name"`
	Duration int             `json:"duration"`
	Artists  []neteaseArtist `json:"artists"`
}

type neteaseArtist struct {
	Name string `json:"name"`
}

type neteaseLyricResp struct {
	LRC      struct{ Lyric string } `json:"lrc"`
	KLyric   struct{ Lyric string } `json:"klyric"` // karaoke / word-level synced
	TLyric   struct{ Lyric string } `json:"tlyric"` // translation
	Uncollected bool                `json:"uncollected"`
}

var neteaseHeaders = map[string]string{
	"Referer": "https://music.163.com/",
}

func fetchFromNetease(artist, title string) *Lyrics {
	// each (artist, title) variant gets the combined-query + title-only
	// (gated) fallback chain. transliterated variants let us hit netease
	// entries filed under the form spotify didn't report.
	for _, pair := range expandSearchPairs(artist, title) {
		queries := []string{pair.Artist + " " + pair.Title}
		if titleSpecificEnough(pair.Title) {
			queries = append(queries, pair.Title)
		}

		for _, q := range queries {
			var search neteaseSearchResp
			searchURL := "https://music.163.com/api/search/get/?" + url.Values{
				"s":      {q},
				"type":   {"1"},
				"limit":  {"10"},
				"offset": {"0"},
			}.Encode()
			if err := httpGetJSON(searchURL, neteaseHeaders, &search); err != nil {
				continue
			}
			songs := search.Result.Songs
			if len(songs) == 0 {
				continue
			}
			// rank against BOTH the variant's terms and the original — covers
			// the case where spotify gives romaji, the variant searched
			// hiragana, and netease returned the kanji form which matches
			// neither directly. the rank function's permissive substring
			// match handles a lot of this; passing the original as a
			// secondary hint catches the rest.
			ranked := neteaseRank(songs, pair.Artist, pair.Title)
			for _, s := range ranked {
				if hit := fetchNeteaseLyric(s.ID); hit != nil {
					hit.Duration = time.Duration(s.Duration) * time.Millisecond
					return hit
				}
			}
		}
	}
	return nil
}

// neteaseRank returns the search hits reordered so the most plausible
// matches come first. exact title + artist match takes priority, then
// title-only matches (covers the case where netease has the song under a
// localized artist name), then everything else in original order.
// neteaseRank returns candidates ordered by match confidence. results in
// the "rest" bucket (neither title nor artist match) are DROPPED — they
// were a false-positive farm: when the actual track had no lyrics in
// netease's database, we'd silently fall through to a totally different
// song by the same artist and serve its lyrics. better to miss than to
// false-positive lyrics on the wrong song.
func neteaseRank(songs []neteaseSong, artist, title string) []neteaseSong {
	var both, titleOnly []neteaseSong
	for _, s := range songs {
		a := ""
		if len(s.Artists) > 0 {
			a = s.Artists[0].Name
		}
		artistOK := artistsRoughlyMatch(a, artist)
		titleOK := titlesRoughlyMatch(s.Name, title)
		switch {
		case artistOK && titleOK:
			both = append(both, s)
		case titleOK:
			titleOnly = append(titleOnly, s)
		}
	}
	return append(both, titleOnly...)
}

func fetchNeteaseLyric(songID int) *Lyrics {
	var resp neteaseLyricResp
	u := "https://music.163.com/api/song/lyric?" + url.Values{
		"id": {itoa(songID)},
		"lv": {"1"},
		"kv": {"1"},
		"tv": {"-1"},
	}.Encode()
	if err := httpGetJSON(u, neteaseHeaders, &resp); err != nil {
		return nil
	}
	if resp.Uncollected {
		return nil
	}
	// prefer synced LRC over karaoke; fall back to karaoke if LRC is empty.
	primary := strings.TrimSpace(resp.LRC.Lyric)
	if primary == "" {
		primary = strings.TrimSpace(resp.KLyric.Lyric)
	}
	if primary == "" {
		return nil
	}
	hit := buildLyricsFromLRC(primary, "")
	// merge netease's translation track when present. tlyric is a parallel
	// LRC sharing the original's timestamps; applyTranslationLRC pairs them
	// up so each line gets its native translation without an extra API call.
	applyTranslationLRC(hit, strings.TrimSpace(resp.TLyric.Lyric))
	return hit
}

// -- qq music (y.qq.com) --
//
// tencent's catalog; another deep well for asian indie & vocaloid. uses
// the legacy soso / fcg_query_lyric_new endpoints which still serve JSON
// without auth as long as the Referer header looks legit.

type qqSearchResp struct {
	Code int `json:"code"`
	Data struct {
		Song struct {
			List []qqSong `json:"list"`
		} `json:"song"`
	} `json:"data"`
}

type qqSong struct {
	SongMID  string     `json:"songmid"`
	SongName string     `json:"songname"`
	Singer   []qqSinger `json:"singer"`
	Interval int        `json:"interval"` // track length in whole seconds
}

type qqSinger struct {
	Name string `json:"name"`
}

type qqLyricResp struct {
	Lyric string `json:"lyric"` // may be base64 depending on params; we ask for plain
	Trans string `json:"trans"`
}

var qqHeaders = map[string]string{
	"Referer": "https://y.qq.com/",
}

func fetchFromQQMusic(artist, title string) *Lyrics {
	// same fallback shape as netease: iterate (artist, title) variants ×
	// (combined-query, title-only-gated) queries.
	for _, pair := range expandSearchPairs(artist, title) {
		queries := []string{pair.Artist + " " + pair.Title}
		if titleSpecificEnough(pair.Title) {
			queries = append(queries, pair.Title)
		}

		for _, q := range queries {
			var search qqSearchResp
			searchURL := "https://c.y.qq.com/soso/fcgi-bin/client_search_cp?" + url.Values{
				"w":      {q},
				"format": {"json"},
				"n":      {"10"},
				"p":      {"1"},
			}.Encode()
			if err := httpGetJSON(searchURL, qqHeaders, &search); err != nil {
				continue
			}
			songs := search.Data.Song.List
			if len(songs) == 0 {
				continue
			}
			ranked := qqRank(songs, pair.Artist, pair.Title)
			for _, s := range ranked {
				if hit := fetchQQLyric(s.SongMID); hit != nil {
					hit.Duration = time.Duration(s.Interval) * time.Second
					return hit
				}
			}
		}
	}
	return nil
}

// qqRank: same false-positive guard as neteaseRank — drop the "neither
// matched" bucket so we never serve a different song's lyrics just
// because the right song was missing them in qq's database.
func qqRank(songs []qqSong, artist, title string) []qqSong {
	var both, titleOnly []qqSong
	for _, s := range songs {
		a := ""
		if len(s.Singer) > 0 {
			a = s.Singer[0].Name
		}
		artistOK := artistsRoughlyMatch(a, artist)
		titleOK := titlesRoughlyMatch(s.SongName, title)
		switch {
		case artistOK && titleOK:
			both = append(both, s)
		case titleOK:
			titleOnly = append(titleOnly, s)
		}
	}
	return append(both, titleOnly...)
}

func fetchQQLyric(songmid string) *Lyrics {
	var resp qqLyricResp
	u := "https://c.y.qq.com/lyric/fcgi-bin/fcg_query_lyric_new.fcg?" + url.Values{
		"songmid":  {songmid},
		"format":   {"json"},
		"nobase64": {"1"},
	}.Encode()
	if err := httpGetJSON(u, qqHeaders, &resp); err != nil {
		return nil
	}
	lyric := strings.TrimSpace(resp.Lyric)
	if lyric == "" {
		return nil
	}
	hit := buildLyricsFromLRC(lyric, "")
	// qq's translation comes as a parallel LRC in resp.Trans. same alignment
	// strategy as netease — match by timestamp with a small tolerance.
	applyTranslationLRC(hit, strings.TrimSpace(resp.Trans))
	return hit
}

// -- tiny helpers --

// itoa avoids pulling in strconv from this file when we only need int→str
// for URL building. one allocation per call, fine for our scale.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
