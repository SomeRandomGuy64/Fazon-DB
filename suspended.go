package main

// Suspended words: the target word (Word field) of a suspended Study Deck card.
//
// A suspended word always counts as UNKNOWN, so it keeps blocking i+1 like any
// unknown word (it overrides review / AnkiMorphs / learned; only an explicit
// manual "known" mark beats it). It is also never a learning target: tag sets,
// "next to mine" (search susp:0) and the JLPT "to mine" list skip it, until
// the word is explicitly allowed (`mdb allow`, or the Suspended tab).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

var (
	// studyDeckSQL = the Study Deck (mine.go) and its subdecks.
	studyDeckSQL = `SELECT id FROM decks WHERE name = '` + strings.ReplaceAll(studyDeck, "'", "''") +
		`' OR name LIKE '` + strings.ReplaceAll(studyDeck, "'", "''") + `::%'`

	// suspendedNotesSQL = Study Deck notes whose cards are all suspended.
	suspendedNotesSQL = `SELECT note_id FROM cards WHERE deck_id IN (` + studyDeckSQL + `)
   GROUP BY note_id HAVING MIN(queue = -1) = 1`
)

// excludedSQL = suspended words not explicitly allowed: never learning targets.
const excludedSQL = `SELECT s.morph_id FROM suspended_words s JOIN morphs m ON m.id = s.morph_id
   WHERE NOT EXISTS (SELECT 1 FROM suspend_marks k WHERE k.base = m.base AND k.reading = m.reading AND k.allowed = 1)`

var (
	reKanji   = regexp.MustCompile(`[一-龯々〆ヶ]`)
	reRubySeg = regexp.MustCompile(`([^\s\[\]]*)\[([^\]]*)\]`)
)

// wordFieldParts splits an MvJ Word field ("予[よ] 定[てい]:L:llh-", "A<br>B",
// "A / B") into (dictionary form, reading) pairs.
func wordFieldParts(field string) [][2]string {
	var out [][2]string
	field = strings.NewReplacer("<br>", "/", "<br/>", "/", "<br />", "/").Replace(field)
	for _, p := range strings.Split(field, "/") {
		p = rePitch.ReplaceAllString(reHTML.ReplaceAllString(p, ""), "")
		p = strings.TrimSpace(strings.ReplaceAll(p, "*", ""))
		base := strings.Join(strings.Fields(reFurigana.ReplaceAllString(p, "")), "")
		if base == "" || base == "-" {
			continue
		}
		reading := strings.Join(strings.Fields(reRubySeg.ReplaceAllString(p, "$2")), "")
		if reKanji.MatchString(reading) { // kanji without furigana: reading unknown
			reading = ""
		}
		out = append(out, [2]string{base, toHiragana(reading)})
	}
	return out
}

type noteMorph struct {
	id                        int64
	base, lemma, reading, pos string
	ignored                   bool
}

// wordMorphs resolves a Word field to morph ids, preferring the words of the
// card's own sentence: its dictionary form, else the tokens spanning it in the
// sentence (に対して → 対する), else the database-wide word.
func wordMorphs(db *sql.DB, nid int64, field string) []int64 {
	var toks [][2]any
	var tj string
	db.QueryRow(`SELECT COALESCE(tokens,'[]') FROM notes WHERE id=?`, nid).Scan(&tj)
	json.Unmarshal([]byte(tj), &toks)
	nm := map[int64]noteMorph{}
	rows, err := db.Query(`SELECT m.id, m.base, COALESCE(m.lemma,''), m.reading, COALESCE(m.pos,''), m.ignored
	   FROM note_morphs x JOIN morphs m ON m.id = x.morph_id WHERE x.note_id = ?`, nid)
	if err == nil {
		for rows.Next() {
			var m noteMorph
			rows.Scan(&m.id, &m.base, &m.lemma, &m.reading, &m.pos, &m.ignored)
			nm[m.id] = m
		}
		rows.Close()
	}
	var out []int64
	for _, wp := range wordFieldParts(field) {
		base, reading := wp[0], wp[1]
		// 1. the dictionary form among the sentence's words, in the card's reading
		// (明日[あす] is not the sentence's 明日 あした)
		var hit int64
		for _, m := range nm {
			if (m.base == base || m.lemma == base) && (reading == "" || toHiragana(m.reading) == reading) {
				hit = m.id
			}
		}
		if hit != 0 {
			out = append(out, hit)
			continue
		}
		// 2. the content words spelling it out in the sentence (に対して → 対する,
		// ある程度 → 程度). The tokens must start and end exactly on the word, so
		// 行先 inside 旅行先 takes neither 旅行 nor 先.
		var text strings.Builder
		var spans [][3]int64 // start, end (bytes), morph id (0 = not a word)
		for _, t := range toks {
			s, _ := t[0].(string)
			id, _ := t[1].(float64)
			st := text.Len()
			text.WriteString(s)
			spans = append(spans, [3]int64{int64(st), int64(text.Len()), int64(id)})
		}
		found := false
		for off := 0; !found; {
			i := strings.Index(text.String()[off:], base)
			if i < 0 {
				break
			}
			at, end := int64(off+i), int64(off+i+len(base))
			off += i + len(base)
			starts, ends := false, false
			var content, kanji []int64
			for _, sp := range spans {
				starts = starts || sp[0] == at
				ends = ends || sp[1] == end
				m, ok := nm[sp[2]]
				if !ok || sp[1] <= at || sp[0] >= end || m.ignored || strings.HasPrefix(m.pos, "助") {
					continue
				}
				// the dictionary reading has to start like the card's (怒[いか]り is not 怒る おこる)
				if r := []rune(toHiragana(m.reading)); reading != "" && reKanji.MatchString(m.base) && !strings.Contains(reading, string(r[:min(2, len(r))])) {
					content = nil
					break
				}
				content = append(content, m.id)
				if reKanji.MatchString(m.base) {
					kanji = append(kanji, m.id)
				}
			}
			if !starts || !ends || len(content) == 0 {
				continue
			}
			if len(content) > 1 && len(kanji) > 0 { // ある程度 → 程度, not the ある
				content = kanji
			}
			out = append(out, content...)
			found = true
		}
		if found {
			continue
		}
		// 3. anywhere in the database (the sentence uses another form)
		var id int64
		db.QueryRow(`SELECT id FROM morphs WHERE (base = ? OR lemma = ?) AND (reading = ? OR ? = '') ORDER BY freq DESC LIMIT 1`,
			base, base, reading, reading).Scan(&id)
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// refreshSuspended rebuilds suspended_words from the cards table. Returns the
// morphs that entered or left the set (their known status needs re-deriving).
func refreshSuspended(db *sql.DB) ([]int64, error) {
	old := map[int64]bool{}
	rows, err := db.Query(`SELECT morph_id FROM suspended_words`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		old[id] = true
	}
	rows.Close()
	type sn struct {
		id   int64
		word string
	}
	var notes []sn
	rows, err = db.Query(`SELECT id, COALESCE(word,'') FROM notes WHERE id IN (` + suspendedNotesSQL + `)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n sn
		rows.Scan(&n.id, &n.word)
		notes = append(notes, n)
	}
	rows.Close()
	cur := map[int64]sn{}
	for _, n := range notes {
		for _, id := range wordMorphs(db, n.id, n.word) {
			if _, ok := cur[id]; !ok {
				cur[id] = n
			}
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	tx.Exec(`DELETE FROM suspended_words`)
	for id, n := range cur {
		tx.Exec(`INSERT INTO suspended_words(morph_id, note_id, word) VALUES(?,?,?)`, id, n.id, n.word)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	var changed []int64
	for id := range cur {
		if !old[id] {
			changed = append(changed, id)
		}
	}
	for id := range old {
		if _, ok := cur[id]; !ok {
			changed = append(changed, id)
		}
	}
	return changed, nil
}

// syncSuspended rebuilds the suspended set and applies what changed (known
// status, i+N, tag sets) outside a sync pass, e.g. when `mdb serve` starts.
func syncSuspended(db *sql.DB) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	changed, err := refreshSuspended(db)
	if err != nil || len(changed) == 0 {
		return err
	}
	flipped, err := refreshKnown(db, changed)
	if err != nil {
		return err
	}
	if _, err := refreshNotes(db, flipped); err != nil {
		return err
	}
	log.Printf("suspended words: %d changed, %d flipped known/unknown", len(changed), len(flipped))
	return computeTagsets(db)
}

// excludedMorphs = the suspended words that are not learning targets.
func excludedMorphs(db *sql.DB) map[int64]bool {
	out := map[int64]bool{}
	rows, err := db.Query(excludedSQL)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		out[id] = true
	}
	return out
}

// allowWord marks a suspended word as a learning target again (allow=true) or
// back to excluded. The word stays unknown either way.
func allowWord(db *sql.DB, base, reading string, allow bool) (int, error) {
	writeMu.Lock()
	defer writeMu.Unlock()
	q := `SELECT m.base, m.reading FROM suspended_words s JOIN morphs m ON m.id = s.morph_id WHERE m.base = ?`
	args := []any{base}
	if reading != "" {
		q += ` AND m.reading = ?`
		args = append(args, reading)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var keys [][2]string
	for rows.Next() {
		var b, r string
		rows.Scan(&b, &r)
		keys = append(keys, [2]string{b, r})
	}
	rows.Close()
	if len(keys) == 0 {
		return 0, fmt.Errorf("%q %q is not a suspended word", base, reading)
	}
	for _, k := range keys {
		if allow {
			db.Exec(`INSERT INTO suspend_marks(base, reading, allowed, at) VALUES(?,?,1,?) ON CONFLICT(base, reading) DO UPDATE SET allowed=1, at=excluded.at`,
				k[0], k[1], time.Now().Unix())
		} else {
			db.Exec(`DELETE FROM suspend_marks WHERE base=? AND reading=?`, k[0], k[1])
		}
	}
	return len(keys), computeTagsets(db)
}

type suspendedRow struct {
	Base     string `json:"base"`
	Reading  string `json:"reading"`
	Word     string `json:"word"` // the Word field as on the card
	NoteID   int64  `json:"nid"`
	Sentence string `json:"sentence"`
	Known    bool   `json:"known"` // marked known by hand
	Allowed  bool   `json:"allowed"`
	Rank     int    `json:"rank,omitempty"`
	IPlus1   int    `json:"i1_cards"`
}

func listSuspended(db *sql.DB) ([]suspendedRow, error) {
	rows, err := db.Query(`SELECT m.base, m.reading, s.word, s.note_id, COALESCE(n.sentence,''), m.known, COALESCE(m.rank,0),
	   EXISTS(SELECT 1 FROM suspend_marks k WHERE k.base = m.base AND k.reading = m.reading AND k.allowed = 1),
	   (SELECT COUNT(*) FROM notes x WHERE x.n_unknown = 1 AND x.unk_morph = m.id)
	   FROM suspended_words s JOIN morphs m ON m.id = s.morph_id LEFT JOIN notes n ON n.id = s.note_id
	   ORDER BY (m.rank IS NULL), m.rank`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []suspendedRow{}
	for rows.Next() {
		var r suspendedRow
		rows.Scan(&r.Base, &r.Reading, &r.Word, &r.NoteID, &r.Sentence, &r.Known, &r.Rank, &r.Allowed, &r.IPlus1)
		out = append(out, r)
	}
	return out, nil
}

// unmatchedSuspended = suspended Study Deck notes none of whose Word maps to a word.
func unmatchedSuspended(db *sql.DB) []map[string]any {
	out := []map[string]any{}
	rows, err := db.Query(`SELECT id, COALESCE(word,''), COALESCE(sentence,'') FROM notes WHERE id IN (` + suspendedNotesSQL + `)
	   AND id NOT IN (SELECT note_id FROM suspended_words)`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var w, s string
		rows.Scan(&id, &w, &s)
		out = append(out, map[string]any{"nid": id, "word": w, "sentence": s})
	}
	return out
}

func printSuspended(db *sql.DB) error {
	rs, err := listSuspended(db)
	if err != nil {
		return err
	}
	for _, r := range rs {
		st := "excluded"
		switch {
		case r.Known:
			st = "known (manual)"
		case r.Allowed:
			st = "allowed"
		}
		fmt.Printf("%-10s %-12s %-15s rank %-6d i+1 %-4d %s\n", r.Base, r.Reading, st, r.Rank, r.IPlus1, r.Word)
	}
	for _, u := range unmatchedSuspended(db) {
		fmt.Printf("UNMATCHED  note %v  %v\n", u["nid"], u["word"])
	}
	return nil
}
