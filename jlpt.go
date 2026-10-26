package main

// JLPT progress: how much of each level's (approximate) vocabulary list is known.
//
// A list word matches a morph by reading plus dictionary form, UniDic lemma
// (綺麗 ~ 奇麗) or a kana spelling. Words the parser splits into short units
// (自転車 = 自転|車, お母さん = お|母|さん) are parsed once with the same model
// (cached in jlpt_parse) and count as known when every part is known.

import (
	"bufio"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

//go:embed jlpt/vocab.tsv
var jlptTSV string

type jlptEntry struct {
	Level   int
	Expr    string
	Reading string
	Meaning string
	cands   [][2]string // (expression, hiragana reading) variants to match
}

var (
	jlptOnce  sync.Once
	jlptWords []jlptEntry
	jlptParen = regexp.MustCompile(`\s*[(（][^)）]*[)）]`)
	jlptMu    sync.Mutex // one jlpt_parse refresh at a time
)

func loadJLPT() []jlptEntry {
	jlptOnce.Do(func() {
		sc := bufio.NewScanner(strings.NewReader(jlptTSV))
		for sc.Scan() {
			line := sc.Text()
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) < 4 {
				continue
			}
			e := jlptEntry{Level: int(f[0][0] - '0'), Expr: f[1], Reading: f[2], Meaning: f[3]}
			// "足; 脚" / "いい; よい" / "～円" / "けっこん (する)" → every expression × reading pair
			strip := func(s string) string {
				return strings.TrimSpace(strings.NewReplacer("～", "", "〜", "", "~", "").Replace(jlptParen.ReplaceAllString(s, "")))
			}
			for _, x := range strings.Split(e.Expr, ";") {
				for _, r := range strings.Split(e.Reading, ";") {
					if x, r = strip(x), toHiragana(strip(r)); x != "" && r != "" {
						e.cands = append(e.cands, [2]string{x, r})
					}
				}
			}
			jlptWords = append(jlptWords, e)
		}
	})
	return jlptWords
}

func toHiragana(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'ァ' && r <= 'ヶ' {
			return r - 0x60
		}
		return r
	}, s)
}

// jlptSplit is one list expression as the parser splits it.
type jlptSplit struct {
	Parts   [][2]string `json:"p"` // (base, reading) of the kept tokens
	Dropped bool        `json:"d"` // a token was filtered out (a numeral in 一つ, 五日)
}

// jlptParts: every list expression, parsed with the current model. Expressions
// missing from the cache (first run, model upgrade) are parsed now with the
// shared worker.
func jlptParts(db *sql.DB) (map[string]jlptSplit, error) {
	jlptMu.Lock()
	defer jlptMu.Unlock()
	fp := modelFingerprint() + "/2"
	parts := map[string]jlptSplit{}
	rows, err := db.Query(`SELECT text, toks FROM jlpt_parse WHERE model=?`, fp)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t, js string
		rows.Scan(&t, &js)
		var p jlptSplit
		json.Unmarshal([]byte(js), &p)
		parts[t] = p
	}
	rows.Close()
	var todo []string
	seen := map[string]bool{}
	for _, e := range loadJLPT() {
		for _, c := range e.cands {
			if _, ok := parts[c[0]]; !ok && !seen[c[0]] {
				seen[c[0]] = true
				todo = append(todo, c[0])
			}
		}
	}
	if len(todo) == 0 {
		return parts, nil
	}
	wk, err := sharedWorker()
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(todo); i += 500 {
		batch := todo[i:min(i+500, len(todo))]
		lines := make([]parseLine, len(batch))
		for j, t := range batch {
			lines[j] = parseLine{ID: int64(j), Text: t}
		}
		res, err := wk.parse(lines)
		if err != nil {
			resetSharedWorker()
			return nil, err
		}
		tx, err := db.Begin()
		if err != nil {
			return nil, err
		}
		for _, r := range res {
			p := jlptSplit{Parts: [][2]string{}}
			for _, t := range r.Toks {
				if len(t) < 7 {
					continue
				}
				if keep, _ := t[6].(float64); keep == 1 && str(t[1]) != "" {
					rd := str(t[2])
					if rd == "" || rd == "?" {
						rd = str(t[1])
					}
					p.Parts = append(p.Parts, [2]string{str(t[1]), rd})
				} else {
					p.Dropped = true
				}
			}
			js, _ := json.Marshal(p)
			text := batch[r.ID]
			parts[text] = p
			tx.Exec(`INSERT INTO jlpt_parse(text, model, toks) VALUES(?,?,?) ON CONFLICT(text) DO UPDATE SET model=excluded.model, toks=excluded.toks`, text, fp, string(js))
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

type jlptMorph struct {
	id                   int64
	base, lemma, reading string
	pos                  string
	rank, i1             int
	known, ignored       bool
}

type jlptPart struct {
	Base    string `json:"base"`
	Reading string `json:"reading"`
	Known   bool   `json:"known"`
}

type jlptWord struct {
	Level   int    `json:"level"`
	Expr    string `json:"expr"`
	Reading string `json:"reading"`
	Meaning string `json:"meaning"`
	// known | unknown (in your cards) | ignored | absent (no card has it) | untracked (numerals etc.)
	Status   string     `json:"status"`
	Base     string     `json:"base,omitempty"` // the word to mark / search: the match, or a compound's first unknown part
	MReading string     `json:"m_reading,omitempty"`
	Rank     int        `json:"rank,omitempty"`
	IPlus1   int        `json:"i1_cards"`
	Parts    []jlptPart `json:"parts,omitempty"` // set when the parser splits the word
}

// jlptMatch resolves every list entry against the current morphs.
func jlptMatch(db *sql.DB) ([]jlptWord, error) {
	parts, err := jlptParts(db)
	if err != nil {
		return nil, err
	}
	byReading := map[string][]*jlptMorph{}
	byKey := map[[2]string]*jlptMorph{}
	byBase := map[string][]*jlptMorph{}
	all := map[int64]*jlptMorph{}
	rows, err := db.Query(`SELECT id, base, COALESCE(lemma,''), reading, COALESCE(pos,''), COALESCE(rank,0), known, ignored FROM morphs`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		m := &jlptMorph{}
		rows.Scan(&m.id, &m.base, &m.lemma, &m.reading, &m.pos, &m.rank, &m.known, &m.ignored)
		r := toHiragana(m.reading)
		byReading[r] = append(byReading[r], m)
		byKey[[2]string{m.base, m.reading}] = m
		byBase[m.base] = append(byBase[m.base], m)
		all[m.id] = m
	}
	rows.Close()
	rows, err = db.Query(`SELECT unk_morph, COUNT(*) FROM notes WHERE n_unknown = 1 GROUP BY unk_morph`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var n int
		rows.Scan(&id, &n)
		if m := all[id]; m != nil {
			m.i1 = n
		}
	}
	rows.Close()

	var out []jlptWord
	for _, e := range loadJLPT() {
		w := jlptWord{Level: e.Level, Expr: e.Expr, Reading: e.Reading, Meaning: e.Meaning, Status: "absent"}
		var hits []*jlptMorph
		seen := map[int64]bool{}
		for _, c := range e.cands {
			// a kana spelling (list or cards) only counts for content words of 2+ kana, so 葉 (は) never matches the particle
			long := utf8.RuneCountInString(c[1]) >= 2
			kanaList := long && toHiragana(c[0]) == c[1] // かばん in the list, 鞄 in your cards
			for _, m := range byReading[c[1]] {
				kana := (m.base == c[1] || kanaList) && long && !strings.HasPrefix(m.pos, "助")
				if !seen[m.id] && (m.base == c[0] || m.lemma == c[0] || kana) {
					seen[m.id] = true
					hits = append(hits, m)
				}
			}
		}
		if len(hits) > 0 {
			var best *jlptMorph
			for _, m := range hits {
				switch {
				case m.known:
					w.Status = "known"
				case !m.ignored && w.Status != "known":
					w.Status = "unknown"
					w.IPlus1 += m.i1
				case w.Status == "absent":
					w.Status = "ignored"
				}
				if best == nil || (m.rank > 0 && (best.rank == 0 || m.rank < best.rank)) {
					best = m
				}
			}
			w.Base, w.MReading, w.Rank = best.base, best.reading, best.rank
		} else {
			jlptCompound(&w, e, parts, byKey, byBase)
		}
		if w.Status == "known" {
			w.IPlus1 = 0
		}
		out = append(out, w)
	}
	return out, nil
}

// jlptCompound scores a word the parser splits: known when every counted part
// is known, absent when some part never occurs in your cards.
func jlptCompound(w *jlptWord, e jlptEntry, parts map[string]jlptSplit, byKey map[[2]string]*jlptMorph, byBase map[string][]*jlptMorph) {
	w.Status = "untracked" // the parser drops (part of) it: numerals (一つ, 五日), names
	for _, c := range e.cands {
		sp := parts[c[0]]
		p := sp.Parts
		if len(p) == 0 || sp.Dropped {
			continue
		}
		var ms []*jlptMorph
		missing := false
		w.Parts = w.Parts[:0]
		for _, t := range p {
			m := byKey[t]
			if m == nil { // same word, other reading in context: take a known one if any
				for _, x := range byBase[t[0]] {
					if m == nil || (x.known && !m.known) {
						m = x
					}
				}
			}
			if m == nil {
				missing = true
				w.Parts = append(w.Parts, jlptPart{Base: t[0], Reading: t[1]})
				continue
			}
			w.Parts = append(w.Parts, jlptPart{Base: m.base, Reading: m.reading, Known: m.known || m.ignored})
			if !m.ignored {
				ms = append(ms, m)
			}
		}
		var unk []*jlptMorph
		for _, m := range ms {
			if !m.known {
				unk = append(unk, m)
			}
			if m.rank > w.Rank || m.rank == 0 { // a compound is as rare as its rarest part
				w.Rank = m.rank
			}
		}
		// one token in another reading (一日 ついたち → いちにち) is a different word
		other := len(p) == 1 && p[0][0] == c[0] && len(ms) == 1 && toHiragana(ms[0].reading) != c[1]
		switch {
		case missing || other:
			w.Status = "absent"
		case len(ms) == 0:
			w.Status = "ignored"
		case len(unk) == 0:
			w.Status = "known"
		default:
			w.Status = "unknown"
			w.Base, w.MReading = unk[0].base, unk[0].reading
			if len(unk) == 1 {
				w.IPlus1 = unk[0].i1
			}
		}
		if len(w.Parts) == 1 { // one part that only matched by another reading: not a split
			w.Parts = nil
		}
		return
	}
}

type jlptLevel struct {
	Level     int `json:"level"`
	Total     int `json:"total"`
	Known     int `json:"known"`
	Minable   int `json:"minable"` // unknown, with at least one i+1 card
	Unknown   int `json:"unknown"` // unknown, in your cards (minable included)
	Ignored   int `json:"ignored"`
	Absent    int `json:"absent"`
	Untracked int `json:"untracked"`
}

func jlptSummary(db *sql.DB) ([]jlptLevel, error) {
	ws, err := jlptMatch(db)
	if err != nil {
		return nil, err
	}
	lv := map[int]*jlptLevel{}
	for n := 1; n <= 5; n++ {
		lv[n] = &jlptLevel{Level: n}
	}
	for _, w := range ws {
		l := lv[w.Level]
		l.Total++
		switch w.Status {
		case "known":
			l.Known++
		case "unknown":
			l.Unknown++
			if w.IPlus1 > 0 {
				l.Minable++
			}
		case "ignored":
			l.Ignored++
		case "untracked":
			l.Untracked++
		default:
			l.Absent++
		}
	}
	out := []jlptLevel{}
	for n := 5; n >= 1; n-- {
		out = append(out, *lv[n])
	}
	return out, nil
}

// jlptList: one level's words. status: all | known | unknown | minable | absent | untracked.
// Most-frequent-in-your-cards first, so the top of the unknown list is what to learn next.
func jlptList(db *sql.DB, level int, status, q string, limit, offset int) ([]jlptWord, error) {
	ws, err := jlptMatch(db)
	if err != nil {
		return nil, err
	}
	out := []jlptWord{}
	q = strings.ToLower(q)
	for _, w := range ws {
		if level != 0 && w.Level != level {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(w.Expr+" "+w.Reading+" "+w.Meaning), q) {
			continue
		}
		if status == "" || status == "all" || w.Status == status || (status == "minable" && w.IPlus1 > 0) {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Rank == 0) != (b.Rank == 0) {
			return a.Rank != 0
		}
		return a.Rank < b.Rank
	})
	if offset >= len(out) {
		return []jlptWord{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func printJLPT(db *sql.DB) error {
	ls, err := jlptSummary(db)
	if err != nil {
		return err
	}
	fmt.Printf("%-4s %6s %6s %7s %8s %7s %8s\n", "", "words", "known", "%", "minable", "unseen", "numerals")
	for _, l := range ls {
		fmt.Printf("N%-3d %6d %6d %6.1f%% %8d %7d %8d\n", l.Level, l.Total, l.Known,
			100*float64(l.Known)/float64(max(1, l.Total-l.Untracked-l.Ignored)), l.Minable, l.Absent, l.Untracked)
	}
	return nil
}
