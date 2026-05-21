// romanize: convert non-latin lyric text to a side-by-side romanization.
//
// japanese gets the heaviest treatment: kagome (pure-go MeCab clone) tags
// each token with its katakana reading, then katakanaToRomaji() flattens
// that to hepburn-ish romaji. this handles kanji-heavy lyrics correctly
// because the dictionary supplies readings for every entry it knows.
//
// chinese passes through go-pinyin which returns syllables with tone
// marks; we space-join them per line.
//
// korean and other scripts have no romanizer here — the line is returned
// unchanged and the renderer simply won't show a paren.
//
// the tokenizer carries a ~15MB embedded dictionary so initialization is
// non-trivial (~1s on first call). we lazy-init via sync.Once so cold
// startup doesn't pay the cost when no asian text is encountered.

package main

import (
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/gojp/kana"
	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
	"github.com/mozillazg/go-pinyin"
)

// -- public api --

// -- bidirectional search-key transliteration --
//
// the lyrics DB providers only know how to look up tracks under the form
// they were indexed with — kanji-indexed entries don't surface for a
// romaji query and vice versa. these helpers generate ALTERNATE search
// keys for an (artist, title) pair so the provider chain can try multiple
// transliterations before giving up.

// transliterateForSearch returns alternative forms of s suitable for a
// lyrics DB search. callers should also keep the original term in their
// search list — this only returns the alternates.
//
//   kana present → japanese romaji (kagome readings, deterministic)
//   pure han     → BOTH japanese reading and chinese pinyin since the
//                  same characters can be either, and we don't know the
//                  song's language from this string alone
//   smashed-romaji → best-effort hiragana via gojp/kana (heuristic)
//
// returns up to a handful of strings; dedupes case-insensitively.
func transliterateForSearch(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var variants []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || strings.EqualFold(v, s) {
			return
		}
		for _, existing := range variants {
			if strings.EqualFold(existing, v) {
				return
			}
		}
		variants = append(variants, v)
	}

	switch {
	case hasJapanese(s):
		// definitely japanese (kana present) — single romaji variant.
		add(japaneseRomaji(s))
	case hasHan(s):
		// ambiguous CJK — could be japanese OR chinese, generate both.
		add(japaneseRomaji(s))
		add(chinesePinyin(s))
	case looksLikeSmashedRomaji(s):
		add(kana.RomajiToHiragana(strings.ToLower(s)))
	}
	return variants
}

// SearchPair represents one variant of an (artist, title) query that a
// provider can try. providers iterate over pairs returned by expandSearchPairs
// so kanji entries can be matched even when spotify reports romaji and
// vice versa.
type SearchPair struct{ Artist, Title string }

// maxSearchPairs caps the variant fan-out so misses don't burn 50+ HTTP
// calls per song. on a 3-provider chain with 4 queries per provider per
// pair, 6 pairs already means up to 72 round-trips for a true miss; more
// than that and the "no lyrics found" message becomes silently slow.
const maxSearchPairs = 6

// expandSearchPairs returns the original pair plus useful transliterated
// variants for both fields. ordering: original first, then pairs in
// confidence order (title-translit first because providers index songs
// primarily by title; artist-translit second).
func expandSearchPairs(artist, title string) []SearchPair {
	pairs := []SearchPair{{artist, title}}
	seen := map[SearchPair]bool{pairs[0]: true}

	add := func(p SearchPair) {
		if p.Title == "" || seen[p] || len(pairs) >= maxSearchPairs {
			return
		}
		seen[p] = true
		pairs = append(pairs, p)
	}

	aVariants := transliterateForSearch(artist)
	tVariants := transliterateForSearch(title)

	// 1. original artist × each title variant — most common case where
	//    spotify gives latin title for an east-asian song.
	for _, tV := range tVariants {
		add(SearchPair{artist, tV})
	}
	// 2. each artist variant × original title — covers the reverse case.
	for _, aV := range aVariants {
		add(SearchPair{aV, title})
	}
	// 3. full cross product of variants. catches cases where both fields
	//    need translit (e.g. romaji-only metadata for a CJK-cataloged
	//    track).
	for _, aV := range aVariants {
		for _, tV := range tVariants {
			add(SearchPair{aV, tV})
		}
	}
	return pairs
}

// looksLikeSmashedRomaji gates the romaji→hiragana heuristic. trigger when:
//   - input is at least 10 runes long (short words are too risky)
//   - input contains no whitespace (real titles at this length always
//     have at least one space)
//   - input is pure ASCII letters/digits (no accents, no punctuation)
//   - vowel ratio is high enough to look like japanese (>= 0.42), since
//     romaji follows a strict (C)V structure and runs ~50% vowels; english
//     long words like "Namelessness" or "Internationalization" cluster
//     much lower and would otherwise sneak through
//   - no rare-in-japanese consonant clusters present ("th", "ph", "wh",
//     "wr", "ck", "ngth", etc.). these never appear in standard hepburn
//     romaji and are strong english signals
//
// catches "kiminozanshihabokunihuru" (50% vowels), "ishikinonaikranke",
// "kimiganaitobokuwoaisenai" while rejecting "Namelessness" (33% vowels +
// "ss" cluster), "Internationalization", "Synchronization".
func looksLikeSmashedRomaji(s string) bool {
	runes := []rune(s)
	if len(runes) < 10 {
		return false
	}
	var vowels int
	for _, r := range runes {
		if r > 0x007e {
			return false
		}
		if unicode.IsSpace(r) {
			return false
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
		switch unicode.ToLower(r) {
		case 'a', 'e', 'i', 'o', 'u':
			vowels++
		}
	}
	if float64(vowels)/float64(len(runes)) < 0.42 {
		return false
	}
	lower := strings.ToLower(s)
	englishOnlyClusters := []string{"th", "ph", "wh", "wr", "ck", "qu", "x"}
	for _, c := range englishOnlyClusters {
		if strings.Contains(lower, c) {
			return false
		}
	}
	return true
}

// -- per-line romanization (for inline display) --

// romanize returns a romanization for a line of lyrics, or "" when the
// line is already mostly latin or no romanizer applies. used by the view
// layer to render "original (romaji)" inline.
func romanize(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return ""
	}
	// already mostly latin? skip the work entirely. threshold is generous
	// so a single accented word or stray non-latin char doesn't trigger
	// the kagome init cost.
	if latinShare(trimmed) > 0.85 {
		return ""
	}

	// japanese (kana + CJK) — kagome handles both.
	if hasJapanese(trimmed) {
		out := strings.TrimSpace(japaneseRomaji(trimmed))
		if out != "" && !strings.EqualFold(out, trimmed) {
			return out
		}
		return ""
	}

	// chinese-only (no kana) — pinyin.
	if hasHan(trimmed) {
		out := strings.TrimSpace(chinesePinyin(trimmed))
		if out != "" && !strings.EqualFold(out, trimmed) {
			return out
		}
		return ""
	}

	// nothing actionable — let the caller skip the paren.
	return ""
}

// -- detection helpers --

// hasJapanese reports whether the line contains hiragana or katakana.
// CJK ideographs alone don't qualify because they could be chinese; the
// presence of kana strongly implies japanese.
func hasJapanese(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return true
		}
	}
	return false
}

// hasHan reports whether the line contains any CJK unified ideograph.
// used as a chinese probe once kana is ruled out.
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// latinShare returns the fraction of letter runes in s that are basic
// latin. punctuation, digits, and whitespace don't count toward the
// denominator so a line like "foo, bar." stays 1.0.
func latinShare(s string) float64 {
	var letters, latin int
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if r < 0x0080 || (unicode.Is(unicode.Latin, r)) {
			latin++
		}
	}
	if letters == 0 {
		return 1.0
	}
	return float64(latin) / float64(letters)
}

// -- japanese --

var (
	tokenizerOnce sync.Once
	tokenizerInst *tokenizer.Tokenizer
	tokenizerErr  error
)

// initTokenizer constructs the kagome tokenizer + dictionary exactly
// once. ~1s on the first call because the IPA dictionary is decompressed
// out of the embedded binary; nanoseconds on subsequent calls.
func initTokenizer() (*tokenizer.Tokenizer, error) {
	tokenizerOnce.Do(func() {
		tokenizerInst, tokenizerErr = tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
	})
	return tokenizerInst, tokenizerErr
}

// japaneseRomaji tokenizes a japanese line and emits hepburn-ish romaji.
// for known tokens it uses the dictionary reading (which renders kanji
// correctly); for unknown tokens it falls back to converting whatever
// kana the surface contained.
func japaneseRomaji(line string) string {
	tk, err := initTokenizer()
	if err != nil || tk == nil {
		return ""
	}
	tokens := tk.Tokenize(line)

	var b strings.Builder
	prevWasWord := false
	for _, tok := range tokens {
		feats := tok.Features()
		// IPA features layout: [pos, pos1, pos2, pos3, conjugation,
		// inflection, base_form, reading, pronunciation]
		reading := ""
		if len(feats) >= 8 {
			reading = feats[7]
		}
		if reading == "*" {
			reading = ""
		}

		surface := tok.Surface
		var piece string
		switch {
		case reading != "":
			piece = katakanaToRomaji(reading)
		case hasJapaneseAny(surface):
			piece = katakanaToRomaji(toKatakana(surface))
		default:
			piece = surface
		}
		if piece == "" {
			continue
		}

		// add a single space between word-like tokens for readability.
		// skip when the previous piece ended in punctuation or this piece
		// starts with one, so we don't write "hello ,".
		if prevWasWord && isWordStart(piece) {
			b.WriteByte(' ')
		}
		b.WriteString(piece)
		prevWasWord = isWordEnd(piece)
	}
	return b.String()
}

func hasJapaneseAny(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func isWordStart(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	}
	return false
}

func isWordEnd(s string) bool {
	if s == "" {
		return false
	}
	// walk runes and remember the last one. avoids the byte-vs-rune
	// indexing bug that crashes on multi-byte tails.
	var last rune
	for _, r := range s {
		last = r
	}
	return unicode.IsLetter(last) || unicode.IsDigit(last)
}

// toKatakana folds hiragana characters to their katakana counterparts so
// our single romaji table only needs one set of keys. non-kana runes pass
// through unchanged.
func toKatakana(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 0x3041 && r <= 0x3096: // hiragana → katakana via fixed offset
			b.WriteRune(r + 0x60)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// -- katakana → hepburn romaji --

// digraphs handles two-character kana clusters (yōon, sokuon-extensions).
// keys are normalized to katakana. order matters: process digraphs before
// single-char lookups.
var katakanaDigraphs = map[string]string{
	"キャ": "kya", "キュ": "kyu", "キョ": "kyo",
	"シャ": "sha", "シュ": "shu", "ショ": "sho", "シェ": "she",
	"チャ": "cha", "チュ": "chu", "チョ": "cho", "チェ": "che",
	"ニャ": "nya", "ニュ": "nyu", "ニョ": "nyo",
	"ヒャ": "hya", "ヒュ": "hyu", "ヒョ": "hyo",
	"ミャ": "mya", "ミュ": "myu", "ミョ": "myo",
	"リャ": "rya", "リュ": "ryu", "リョ": "ryo",
	"ギャ": "gya", "ギュ": "gyu", "ギョ": "gyo",
	"ジャ": "ja", "ジュ": "ju", "ジョ": "jo", "ジェ": "je",
	"ビャ": "bya", "ビュ": "byu", "ビョ": "byo",
	"ピャ": "pya", "ピュ": "pyu", "ピョ": "pyo",
	"ファ": "fa", "フィ": "fi", "フェ": "fe", "フォ": "fo", "フュ": "fyu",
	"ウィ": "wi", "ウェ": "we", "ウォ": "wo",
	"ヴァ": "va", "ヴィ": "vi", "ヴェ": "ve", "ヴォ": "vo",
	"ティ": "ti", "トゥ": "tu", "ディ": "di", "ドゥ": "du",
	"ツァ": "tsa", "ツィ": "tsi", "ツェ": "tse", "ツォ": "tso",
}

var katakanaMono = map[rune]string{
	'ア': "a", 'イ': "i", 'ウ': "u", 'エ': "e", 'オ': "o",
	'カ': "ka", 'キ': "ki", 'ク': "ku", 'ケ': "ke", 'コ': "ko",
	'サ': "sa", 'シ': "shi", 'ス': "su", 'セ': "se", 'ソ': "so",
	'タ': "ta", 'チ': "chi", 'ツ': "tsu", 'テ': "te", 'ト': "to",
	'ナ': "na", 'ニ': "ni", 'ヌ': "nu", 'ネ': "ne", 'ノ': "no",
	'ハ': "ha", 'ヒ': "hi", 'フ': "fu", 'ヘ': "he", 'ホ': "ho",
	'マ': "ma", 'ミ': "mi", 'ム': "mu", 'メ': "me", 'モ': "mo",
	'ヤ': "ya", 'ユ': "yu", 'ヨ': "yo",
	'ラ': "ra", 'リ': "ri", 'ル': "ru", 'レ': "re", 'ロ': "ro",
	'ワ': "wa", 'ヰ': "wi", 'ヱ': "we", 'ヲ': "wo", 'ン': "n",
	'ガ': "ga", 'ギ': "gi", 'グ': "gu", 'ゲ': "ge", 'ゴ': "go",
	'ザ': "za", 'ジ': "ji", 'ズ': "zu", 'ゼ': "ze", 'ゾ': "zo",
	'ダ': "da", 'ヂ': "ji", 'ヅ': "zu", 'デ': "de", 'ド': "do",
	'バ': "ba", 'ビ': "bi", 'ブ': "bu", 'ベ': "be", 'ボ': "bo",
	'パ': "pa", 'ピ': "pi", 'プ': "pu", 'ペ': "pe", 'ポ': "po",
	'ヴ': "vu",
	'ァ': "a", 'ィ': "i", 'ゥ': "u", 'ェ': "e", 'ォ': "o",
	'ャ': "ya", 'ュ': "yu", 'ョ': "yo",
}

// katakanaToRomaji converts a katakana string to hepburn-style romaji.
// handles sokuon (ッ) by doubling the next consonant and chouonpu (ー)
// by repeating the previous vowel. unknown runes pass through unchanged.
func katakanaToRomaji(s string) string {
	runes := []rune(s)
	var b strings.Builder
	pendingDouble := false
	prevVowel := byte(0)
	for i := 0; i < len(runes); i++ {
		// long-vowel marker: extend the prior vowel.
		if runes[i] == 'ー' {
			if prevVowel != 0 {
				b.WriteByte(prevVowel)
			}
			continue
		}
		// sokuon: double the consonant of the next syllable.
		if runes[i] == 'ッ' {
			pendingDouble = true
			continue
		}

		var syllable string
		// try digraph first
		if i+1 < len(runes) {
			pair := string(runes[i : i+2])
			if rom, ok := katakanaDigraphs[pair]; ok {
				syllable = rom
				i++
			}
		}
		if syllable == "" {
			if rom, ok := katakanaMono[runes[i]]; ok {
				syllable = rom
			} else {
				// not a kana — passthrough. resets sokuon/long-vowel state.
				if pendingDouble {
					pendingDouble = false
				}
				b.WriteRune(runes[i])
				prevVowel = 0
				continue
			}
		}

		// handle moraic n before m/b/p → render as "n" still (hepburn often
		// uses "m" before b/m/p, but "n" is more searchable and unambiguous).

		if pendingDouble && len(syllable) > 0 {
			b.WriteByte(syllable[0])
			pendingDouble = false
		}
		b.WriteString(syllable)
		// remember the final vowel so a following ー can extend it.
		prevVowel = lastVowelByte(syllable)
	}
	// clean up double spaces from passthrough runes
	out := regexp.MustCompile(`\s+`).ReplaceAllString(b.String(), " ")
	return strings.TrimSpace(out)
}

func lastVowelByte(s string) byte {
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case 'a', 'i', 'u', 'e', 'o':
			return s[i]
		}
	}
	return 0
}

// -- chinese --

// chinesePinyin renders han characters as space-separated pinyin syllables
// with tone marks. non-han runes pass through unchanged so the surrounding
// punctuation and any embedded latin words stay readable.
func chinesePinyin(line string) string {
	args := pinyin.NewArgs()
	args.Style = pinyin.Tone

	var b strings.Builder
	runes := []rune(line)
	prevPinyin := false
	for _, r := range runes {
		if unicode.Is(unicode.Han, r) {
			py := pinyin.SinglePinyin(r, args)
			if len(py) == 0 || py[0] == "" {
				b.WriteRune(r)
				prevPinyin = false
				continue
			}
			if prevPinyin {
				b.WriteByte(' ')
			}
			b.WriteString(py[0])
			prevPinyin = true
			continue
		}
		if prevPinyin && !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
		prevPinyin = false
	}
	return strings.TrimSpace(b.String())
}
