package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseLRCAndActive(t *testing.T) {
	lrc := "[ti:Test]\n[ar:Foo]\n[00:01.50]first line\n[00:05.20]second line\n[00:10.00][00:15.00]repeated\n[01:00.123]minute mark\n"
	lines := parseLRC(lrc)
	if len(lines) != 5 {
		t.Fatalf("expected 5 entries (with multi-stamp expansion), got %d", len(lines))
	}
	if lines[0].Text != "first line" || lines[0].At != 1500*time.Millisecond {
		t.Fatalf("first line bad: %+v", lines[0])
	}
	if lines[4].At != 60*time.Second+123*time.Millisecond {
		t.Fatalf("minute mark bad: %+v", lines[4])
	}
	if activeLineIndex(lines, 6*time.Second) != 1 {
		t.Fatalf("active@6s wrong")
	}
	if activeLineIndex(lines, 12*time.Second) != 2 {
		t.Fatalf("active@12s wrong")
	}
	if activeLineIndex(lines, 100*time.Millisecond) != -1 {
		t.Fatalf("active@100ms should be -1 (before first), got something else")
	}
}

func TestParseNowPlaying(t *testing.T) {
	a, ti := parseNowPlaying("Queen - Bohemian Rhapsody")
	if a != "Queen" || ti != "Bohemian Rhapsody" {
		t.Fatalf("got %q / %q", a, ti)
	}
	a, ti = parseNowPlaying("PAUSED")
	if a != "" || ti != "" {
		t.Fatal("paused should yield empty")
	}
	a, ti = parseNowPlaying("Foo - Bar - Baz")
	if a != "Foo" || ti != "Bar - Baz" {
		t.Fatalf("multi-dash split wrong: %q / %q", a, ti)
	}
}

func TestStripLRCHeaders(t *testing.T) {
	in := "[ti:Bohemian]\n[ar:Queen]\n\nIs this the real life?\nIs this just fantasy?\n"
	out := stripLRCHeaders(in)
	if out != "Is this the real life?\nIs this just fantasy?" {
		t.Fatalf("unexpected: %q", out)
	}
}

func TestNormalizeAndMatch(t *testing.T) {
	if !artistsRoughlyMatch("The Beatles", "Beatles") {
		t.Fatal("The Beatles vs Beatles should match")
	}
	if !artistsRoughlyMatch("Beyoncé", "Beyonce") {
		t.Fatal("accent-insensitive match expected")
	}
	if !artistsRoughlyMatch("AC/DC", "ACDC") {
		t.Fatal("punctuation-stripped match expected")
	}
	if artistsRoughlyMatch("Queen", "Adele") {
		t.Fatal("Queen vs Adele should NOT match")
	}
	if !titlesRoughlyMatch("Bohemian Rhapsody", "Bohemian Rhapsody (Remastered 2011)") {
		t.Fatal("title with parenthetical decoration should match base title")
	}
	if !titlesRoughlyMatch("群青", "群青 - THE FIRST TAKE") {
		t.Fatal("title with hyphen suffix should match base title")
	}
}

func TestNeteaseRanking(t *testing.T) {
	songs := []neteaseSong{
		{ID: 1, Name: "Cover", Artists: []neteaseArtist{{Name: "Random"}}},
		{ID: 2, Name: "Bohemian Rhapsody", Artists: []neteaseArtist{{Name: "Queen"}}},
		{ID: 3, Name: "Bohemian Rhapsody", Artists: []neteaseArtist{{Name: "Other"}}},
	}
	ranked := neteaseRank(songs, "Queen", "Bohemian Rhapsody")
	if ranked[0].ID != 2 {
		t.Fatalf("expected ID 2 first (artist+title match), got %d", ranked[0].ID)
	}
	if ranked[1].ID != 3 {
		t.Fatalf("expected ID 3 second (title only), got %d", ranked[1].ID)
	}
	if ranked[2].ID != 1 {
		t.Fatalf("expected ID 1 last (no match), got %d", ranked[2].ID)
	}
}

func TestBuildLyricsFromLRC(t *testing.T) {
	if hit := buildLyricsFromLRC("", ""); hit != nil {
		t.Fatal("expected nil for empty inputs")
	}
	hit := buildLyricsFromLRC("[00:01.00]hello\n[00:02.00]world\n", "")
	if hit == nil || !hit.Found {
		t.Fatal("expected found")
	}
	if len(hit.Synced) != 2 {
		t.Fatalf("expected 2 synced lines, got %d", len(hit.Synced))
	}
	if hit.Plain == "" {
		t.Fatal("plain should be derived from LRC when omitted")
	}
}

func TestInsertGapMarkers(t *testing.T) {
	l := &Lyrics{
		Duration: 60 * time.Second,
		Synced: []LRCLine{
			{At: 12 * time.Second, Text: "first"},  // intro 12s ≥ 8s → marker
			{At: 14 * time.Second, Text: "second"}, // 2s gap → no marker
			{At: 30 * time.Second, Text: "third"},  // 16s gap → marker @ midpoint 22s
			{At: 45 * time.Second, Text: "last"},   // 15s gap to end → marker @ midpoint 52.5s
		},
	}
	insertGapMarkers(l)
	// expected: 4 markers (intro + 2 inter-line + outro)
	//   marker @ 0s   (intro)
	//   first @ 12s
	//   second @ 14s
	//   marker @ 22s  (midpoint of 14→30)
	//   third @ 30s
	//   marker @ 37.5s (midpoint of 30→45)
	//   last @ 45s
	//   marker @ 52.5s (midpoint of 45→60)
	markerCount := 0
	for _, line := range l.Synced {
		if line.Text == gapMarkerText {
			markerCount++
		}
	}
	if markerCount != 4 {
		t.Fatalf("expected 4 markers, got %d: %+v", markerCount, l.Synced)
	}
	// verify midpoint placement specifically (vs old `prev + 2s lead`)
	for _, line := range l.Synced {
		if line.Text != gapMarkerText {
			continue
		}
		if line.At == 0 {
			continue // intro is at 0s, not a midpoint case
		}
		// inter-line markers should be at a midpoint we expect
		expected := map[time.Duration]bool{
			22 * time.Second:                    true,
			37*time.Second + 500*time.Millisecond: true,
			52*time.Second + 500*time.Millisecond: true,
		}
		if !expected[line.At] {
			t.Fatalf("unexpected marker timestamp %v (expected one of {22s, 37.5s, 52.5s})", line.At)
		}
	}
	if l.Synced[0].Text != gapMarkerText || l.Synced[0].At != 0 {
		t.Fatalf("first entry should be intro marker at 0s, got %+v", l.Synced[0])
	}
}

func TestApplyTranslationLRC(t *testing.T) {
	l := &Lyrics{
		Synced: []LRCLine{
			{At: 1 * time.Second, Text: "嗚呼いつもの様に"},
			{At: 4 * time.Second, Text: "過ぎる日々にあくびが出る"},
			{At: 8 * time.Second, Text: "さんざめく夜越え今日も"},
		},
	}
	// translation timestamps drift by up to 100ms from originals — within tolerance
	trans := "[00:01.100]Ah, as usual\n[00:04.000]I yawn at the passing days\n[00:08.200]The rustling night again today\n"
	applyTranslationLRC(l, trans)
	if l.Synced[0].Translation != "Ah, as usual" {
		t.Fatalf("first line translation wrong: %q", l.Synced[0].Translation)
	}
	if l.Synced[2].Translation == "" {
		t.Fatalf("third line translation should be set despite +200ms drift")
	}
}

func TestFlattenSynced(t *testing.T) {
	synced := []LRCLine{
		{Text: "Hello"},                                           // 1 row: text only
		{Text: "嗚呼", Romaji: "aa"},                                // 2 rows
		{Text: "群青", Romaji: "gunjou", Translation: "Group Blue"}, // 3 rows
		// blank text rows are defensively skipped here too (normally dropped
		// upstream by dropBlankLines).
		{Text: "", Romaji: "", Translation: ""},
	}
	// big width so nothing wraps — keeps the row counts exactly what the
	// test wants to assert (1 per text + 1 per romaji + 1 per translation).
	rows := flattenSynced(synced, 200)
	wantCounts := []int{1, 2, 3, 0}
	got := make([]int, len(synced))
	for _, r := range rows {
		got[r.srcIdx]++
	}
	for i := range wantCounts {
		if got[i] != wantCounts[i] {
			t.Fatalf("srcIdx %d: want %d rows, got %d", i, wantCounts[i], got[i])
		}
	}
}

func TestDropBlankLines(t *testing.T) {
	l := &Lyrics{
		Synced: []LRCLine{
			{At: 1 * time.Second, Text: "first"},
			{At: 2 * time.Second, Text: ""}, // bare timestamp, dropped
			{At: 3 * time.Second, Text: "   "}, // whitespace-only, also dropped
			{At: 4 * time.Second, Text: "last"},
		},
	}
	dropBlankLines(l)
	if len(l.Synced) != 2 {
		t.Fatalf("expected 2 entries after drop, got %d: %+v", len(l.Synced), l.Synced)
	}
	if l.Synced[0].Text != "first" || l.Synced[1].Text != "last" {
		t.Fatalf("wrong entries kept: %+v", l.Synced)
	}
}

func TestWrapForWidth(t *testing.T) {
	// ASCII with spaces — word-boundary breaks preferred
	got := wrapForWidth("the quick brown fox jumps over the lazy dog", 20)
	for _, line := range got {
		if len(line) > 20 {
			t.Fatalf("line too wide: %q (%d)", line, len(line))
		}
	}
	if len(got) < 2 {
		t.Fatalf("expected wrap into >=2 lines, got %d: %+v", len(got), got)
	}
	if got[0] != "the quick brown fox" {
		t.Fatalf("first line broken at wrong word: %q", got[0])
	}

	// CJK without spaces — hard-split by character
	got = wrapForWidth("嗚呼いつもの様にだ過ぎる日々にあくびが出る", 10)
	for _, line := range got {
		w := 0
		for _, r := range line {
			if r == ' ' {
				continue
			}
			// each CJK char = 2 cells, target is 10 cells
			w += 2
		}
		if w > 10 {
			t.Fatalf("CJK line too wide: %q (cells %d)", line, w)
		}
	}
	if len(got) < 2 {
		t.Fatalf("CJK should have wrapped: %+v", got)
	}

	// single word longer than width — hard split applies
	got = wrapForWidth("superlongunbreakableword", 5)
	if len(got) < 2 {
		t.Fatalf("long word should be hard-split, got %+v", got)
	}

	// fits in one — return original
	got = wrapForWidth("short", 20)
	if len(got) != 1 || got[0] != "short" {
		t.Fatalf("short string should not wrap: %+v", got)
	}
}

func TestFlattenSyncedWrap(t *testing.T) {
	// long English text with romaji + translation — expect multiple wrap
	// rows per kind. with inner=20, "the quick brown fox jumps over the lazy
	// dog" wraps into 3 lines for both text + translation.
	synced := []LRCLine{
		{
			Text:        "the quick brown fox jumps over the lazy dog",
			Translation: "the quick brown fox jumps over the lazy dog",
		},
	}
	rows := flattenSynced(synced, 20)
	textRows := 0
	transRows := 0
	for _, r := range rows {
		switch r.kind {
		case lyricKindText:
			textRows++
		case lyricKindTranslation:
			transRows++
		}
	}
	if textRows < 2 {
		t.Fatalf("expected text to wrap into >=2 rows, got %d", textRows)
	}
	if transRows < 2 {
		t.Fatalf("expected translation to wrap into >=2 rows, got %d", transRows)
	}
	// first text row should be marked isFirst, others not
	firstSeen := false
	for _, r := range rows {
		if r.kind != lyricKindText {
			continue
		}
		if r.isFirst {
			if firstSeen {
				t.Fatal("multiple text rows marked isFirst within one LRC entry")
			}
			firstSeen = true
		}
	}
	if !firstSeen {
		t.Fatal("expected first text row to be marked isFirst")
	}
}

func TestTruncateForWidth(t *testing.T) {
	// pure ASCII
	if truncateForWidth("Hello World", 5) != "He..." {
		t.Fatalf("ascii truncation wrong: %q", truncateForWidth("Hello World", 5))
	}
	// CJK chars take 2 cells each. "群青" (4 cells) + "..." (3) = 7 cells
	got := truncateForWidth("群青夜空", 7)
	if got != "群青..." {
		t.Fatalf("CJK width-7 truncation wrong: got %q", got)
	}
	// fits exactly — no truncation, no ellipsis
	got = truncateForWidth("群青", 4)
	if got != "群青" {
		t.Fatalf("exact-fit CJK should return original: got %q", got)
	}
}

func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", 42: "42", -7: "-7", 1234567890: "1234567890"}
	for n, want := range cases {
		if got := itoa(n); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestKatakanaToRomaji(t *testing.T) {
	// chouonpu (ー) extends the previous vowel ("スーパー" → "suupaa"),
	// sokuon (ッ) doubles the next consonant ("ガッコウ" → "gakkou"),
	// digraphs like "ジャ" → "ja" and "ヴァ" → "va" are handled before
	// per-character lookup.
	cases := map[string]string{
		"バナナ":     "banana",
		"カンジ":     "kanji",
		"トウキョウ":   "toukyou",
		"ジャンプ":    "janpu",
		"ガッコウ":    "gakkou",
		"ヴァイオリン":  "vaiorin",
		"スーパー":    "suupaa",
		"シャシン":    "shashin",
		"チョコレート":  "chokoreeto",
	}
	for in, want := range cases {
		got := katakanaToRomaji(in)
		if got != want {
			t.Fatalf("katakana %q → %q, want %q", in, got, want)
		}
	}
}

func TestToKatakana(t *testing.T) {
	if toKatakana("ばなな") != "バナナ" {
		t.Fatalf("hiragana → katakana failed: %q", toKatakana("ばなな"))
	}
	if toKatakana("こんにちは、world") != "コンニチハ、world" {
		t.Fatalf("mixed conversion: %q", toKatakana("こんにちは、world"))
	}
}

func TestRomanize(t *testing.T) {
	// pure-latin lines should return empty (no need to romanize)
	if r := romanize("Hello world"); r != "" {
		t.Fatalf("expected empty for latin, got %q", r)
	}
	// hiragana-only line. tokenizer init happens here so this test is slow
	// on first run (~1s) — that's the cost of the embedded dictionary.
	r := romanize("こんにちは")
	if !strings.Contains(r, "konnichiha") && !strings.Contains(r, "konnichi") {
		t.Fatalf("japanese romanize sanity check failed: %q", r)
	}
	// chinese (no kana) → pinyin path
	r = romanize("你好世界")
	if !strings.Contains(r, "nǐ") && !strings.Contains(r, "ni") {
		t.Fatalf("chinese pinyin sanity check failed: %q", r)
	}
}

func TestLooksLikeSmashedRomaji(t *testing.T) {
	// realistic cases we WANT to detect as smashed romaji
	yes := []string{
		"ishikinonaikranke",
		"kiminozanshihabokunihuru",
		"kimiganaitobokuwoaisenai",
	}
	// realistic cases we MUST NOT misclassify (spotify-reported english titles)
	no := []string{
		"YOASOBI",                        // too short
		"Bohemian Rhapsody",              // has space
		"Hello",                          // too short
		"Beyoncé",                        // non-ASCII
		"群青",                            // CJK
		"kimi no zanshi ha boku ni huru", // has spaces
		"Namelessness",                   // English long word — low vowel ratio + "ss" cluster
	}
	// NOTE: edge cases like "Internationalization" (50% vowel, no english-only
	// clusters) will currently slip through the gate. accepted false positive;
	// the worst case is one wasted HTTP roundtrip on a song that wouldn't
	// have matched anyway.
	for _, s := range yes {
		if !looksLikeSmashedRomaji(s) {
			t.Fatalf("%q should look smashed", s)
		}
	}
	for _, s := range no {
		if looksLikeSmashedRomaji(s) {
			t.Fatalf("%q should NOT look smashed", s)
		}
	}
}

func TestExpandSearchPairs(t *testing.T) {
	// CJK title → expands to add a romaji variant; original stays first.
	pairs := expandSearchPairs("YOASOBI", "群青")
	if pairs[0].Title != "群青" {
		t.Fatalf("original pair should be first, got %+v", pairs[0])
	}
	hasRomaji := false
	for _, p := range pairs[1:] {
		if strings.Contains(strings.ToLower(p.Title), "gunjou") {
			hasRomaji = true
		}
	}
	if !hasRomaji {
		t.Fatalf("expected a romaji 'gunjou' variant in pairs: %+v", pairs)
	}

	// smashed-romaji title → expands to add a hiragana variant.
	pairs = expandSearchPairs("ishikinonaikranke", "kiminozanshihabokunihuru")
	hasHiragana := false
	for _, p := range pairs[1:] {
		for _, r := range p.Title {
			if r >= 0x3040 && r <= 0x309F {
				hasHiragana = true
				break
			}
		}
	}
	if !hasHiragana {
		t.Fatalf("expected a hiragana variant for smashed romaji: %+v", pairs)
	}

	// already-latin, normal-looking title → no transliteration variant.
	pairs = expandSearchPairs("Queen", "Bohemian Rhapsody")
	if len(pairs) != 1 {
		t.Fatalf("normal latin pair should not expand, got %+v", pairs)
	}
}

func TestLatinShare(t *testing.T) {
	if latinShare("Hello, world!") < 0.99 {
		t.Fatal("pure ASCII should be 1.0")
	}
	if latinShare("こんにちは") != 0 {
		t.Fatalf("pure japanese should be 0, got %v", latinShare("こんにちは"))
	}
	if latinShare("hello こんにちは") > 0.6 {
		t.Fatalf("mixed should be ~0.5, got %v", latinShare("hello こんにちは"))
	}
}
