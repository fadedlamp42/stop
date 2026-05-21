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
	"time"
)

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

	// google translate's web/mobile API: gtx client, auto-detect source
	// (sl=auto), target english (tl=en), retrieve translation (dt=t).
	u := "https://translate.googleapis.com/translate_a/single?" + url.Values{
		"client": {"gtx"},
		"sl":     {"auto"},
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
