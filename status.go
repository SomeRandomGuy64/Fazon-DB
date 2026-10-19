package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type searchRow struct {
	NoteID     int64    `json:"nid"`
	Deck       string   `json:"deck"`
	Sentence   string   `json:"sentence"`
	Translation string  `json:"translation"`
	NUnknown   int      `json:"n_unknown"`
	Unknown    []string `json:"unknown"`
	UnkRank    *int     `json:"unk_rank,omitempty"`
	InAnki     bool     `json:"in_anki"`
	Audio      string   `json:"audio"`
	Image      string   `json:"image"`
	Tags       []string `json:"tags"`
}

// search runs a query; i+1 results come back in priority order (the unknown
// word's frequency rank), everything else by note id.
func search(db *sql.DB, q string, limit, offset int) ([]searchRow, error) {
	where, args, err := buildWhere(db, q)
	if err != nil {
		return nil, err
	}
	args = append(args, limit, offset)
	rows, err := db.Query(`SELECT n.id, COALESCE(d.name,''), COALESCE(n.sentence,''), COALESCE(n.notes,''), n.n_unknown, mu.rank,
	   n.in_anki, COALESCE(n.sentence_audio,''), COALESCE(n.image,'')
	   FROM notes n LEFT JOIN decks d ON d.id = n.deck_id LEFT JOIN morphs mu ON mu.id = n.unk_morph
	   WHERE `+where+` ORDER BY (mu.rank IS NULL), mu.rank, n.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	var out []searchRow
	for rows.Next() {
		var r searchRow
		var rank sql.NullInt64
		var inAnki int
		var audio, img string
		rows.Scan(&r.NoteID, &r.Deck, &r.Sentence, &r.Translation, &r.NUnknown, &rank, &inAnki, &audio, &img)
		if rank.Valid {
			v := int(rank.Int64)
			r.UnkRank = &v
		}
		r.InAnki = inAnki == 1
		r.Audio, r.Image = audioName(audio), imgName(img)
		out = append(out, r)
	}
	rows.Close()
	for i := range out {
		out[i].Unknown = unknownWords(db, out[i].NoteID)
		out[i].Tags = noteTags(db, out[i].NoteID)
	}
	return out, nil
}

func unknownWords(db *sql.DB, nid int64) []string {
	rows, err := db.Query(`SELECT m.base || CASE WHEN m.reading != m.base THEN '【' || m.reading || '】' ELSE '' END
	   FROM note_morphs nm JOIN morphs m ON m.id = nm.morph_id WHERE nm.note_id = ? AND m.known = 0 AND m.ignored = 0`, nid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// noteTags = imported tags + tag-set tags + derived status tag.
func noteTags(db *sql.DB, nid int64) []string {
	out := []string{}
	rows, err := db.Query(`SELECT tag FROM tagset_assign WHERE note_id=? UNION ALL SELECT tag FROM note_tags WHERE note_id=?`, nid, nid)
	if err == nil {
		for rows.Next() {
			var t string
			rows.Scan(&t)
			out = append(out, t)
		}
		rows.Close()
	}
	var n int
	db.QueryRow(`SELECT n_unknown FROM notes WHERE id=?`, nid).Scan(&n)
	switch {
	case n == 0:
		out = append(out, "_card-status::i+0")
	case n == 1:
		out = append(out, "_card-status::i+1")
	default:
		out = append(out, "_card-status::i+≥2")
	}
	return out
}

type statusInfo struct {
	Notes        int            `json:"notes"`
	PoolNotes    int            `json:"pool_notes"`
	NotInAnki    int            `json:"not_in_anki"`
	Parsed       int            `json:"parsed_current_model"`
	Stale        int            `json:"parsed_older_model_or_unparsed"`
	Model        string         `json:"model"`
	Words        int            `json:"words"`
	KnownWords   int            `json:"known_words"`        // words that don't block i+1 (AnkiMorphs-style)
	LearnedWords int            `json:"learned_target_words"` // distinct target words on studied cards
	Ignored      int            `json:"ignored_words"`
	KnownBySrc   map[string]int `json:"known_by_source"`
	IPlus        map[string]int `json:"i_plus"`
	Tagsets      []tagsetInfo   `json:"tagsets"`
	LastImport   string         `json:"last_import"`
	LastSync     string         `json:"last_sync"`
	LastBackup   string         `json:"last_backup"`
	Coverage     float64        `json:"token_coverage_pct"`
}

type tagsetInfo struct {
	Prefix  string `json:"prefix"`
	Name    string `json:"name"`
	Targets int    `json:"targets"`
	Placed  int    `json:"placed"`
	Tags    int    `json:"tags"`
}

func getStatus(db *sql.DB) statusInfo {
	s := statusInfo{KnownBySrc: map[string]int{}, IPlus: map[string]int{}}
	fp := modelFingerprint()
	s.Model = fp
	db.QueryRow(`SELECT COUNT(*), SUM(in_anki=0), SUM(parsed_model=?), SUM(parsed_model IS NULL OR parsed_model!=?) FROM notes`, fp, fp).
		Scan(&s.Notes, &s.NotInAnki, &s.Parsed, &s.Stale)
	db.QueryRow(`SELECT COUNT(*) FROM notes n JOIN decks d ON d.id=n.deck_id WHERE d.is_pool=1`).Scan(&s.PoolNotes)
	db.QueryRow(`SELECT SUM(ignored = 0), SUM(known), SUM(ignored) FROM morphs WHERE freq > 0`).Scan(&s.Words, &s.KnownWords, &s.Ignored)
	s.LearnedWords = learnedTargetWords(db)
	rows, _ := db.Query(`SELECT COALESCE(known_src,'?'), COUNT(*) FROM morphs WHERE known=1 AND freq > 0 GROUP BY 1`)
	for rows != nil && rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		s.KnownBySrc[k] = n
	}
	if rows != nil {
		rows.Close()
	}
	rows, _ = db.Query(`SELECT CASE WHEN n_unknown >= 2 THEN 'i+≥2' ELSE 'i+' || n_unknown END, COUNT(*) FROM notes GROUP BY 1`)
	for rows != nil && rows.Next() {
		var k string
		var n int
		rows.Scan(&k, &n)
		s.IPlus[k] = n
	}
	if rows != nil {
		rows.Close()
	}
	var tot, kn sql.NullFloat64
	db.QueryRow(`SELECT SUM(freq), SUM(CASE WHEN known=1 THEN freq END) FROM morphs WHERE ignored = 0`).Scan(&tot, &kn)
	if tot.Float64 > 0 {
		s.Coverage = 100 * kn.Float64 / tot.Float64
	}
	rows, _ = db.Query(`SELECT t.prefix, COALESCE(t.name,''), (SELECT COUNT(*) FROM tagset_targets WHERE tagset_id=t.id),
	   (SELECT COUNT(DISTINCT rank) FROM tagset_assign WHERE tagset_id=t.id), (SELECT COUNT(*) FROM tagset_assign WHERE tagset_id=t.id)
	   FROM tagsets t ORDER BY t.prefix`)
	for rows != nil && rows.Next() {
		var t tagsetInfo
		rows.Scan(&t.Prefix, &t.Name, &t.Targets, &t.Placed, &t.Tags)
		s.Tagsets = append(s.Tagsets, t)
	}
	if rows != nil {
		rows.Close()
	}
	s.LastImport, s.LastSync, s.LastBackup = getMeta(db, "last_import"), getMeta(db, "last_sync"), getMeta(db, "last_backup")
	return s
}

func printStatus(db *sql.DB) error {
	b, _ := json.MarshalIndent(getStatus(db), "", "  ")
	fmt.Println(string(b))
	return nil
}

// learnedTargetWords counts the distinct target words (Word field) on notes
// with a studied card — the real vocabulary number, like count_known_words.py.
func learnedTargetWords(db *sql.DB) int {
	rows, err := db.Query(`SELECT word FROM notes n WHERE word != '' AND EXISTS (SELECT 1 FROM cards c WHERE c.note_id = n.id AND c.type != 0)`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var w string
		rows.Scan(&w)
		for _, part := range strings.Split(w, "/") { // multi-unit entries are joined by "/"
			if t := parseText("Word", "", part); t != "" {
				seen[t] = true
			}
		}
	}
	return len(seen)
}
