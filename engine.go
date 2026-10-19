package main

// The AnkiMorphs replacement. Instead of a whole-collection recalc, every
// derived value is kept up to date incrementally:
//
//   card reviewed / note edited
//     -> known status of that note's words re-derived      (refreshKnown)
//     -> notes containing a word whose status flipped get
//        their i+N recounted                                (refreshNotes)
//     -> tag sets recomputed from the i+1 index            (computeTagsets)
//
// A word (morph) is known when any of these hold, mirroring the AnkiMorphs
// settings in use (lemma evaluation, known interval 1):
//   manual   - marked known in mining-db (an explicit "unknown" mark wins too)
//   review   - it occurs on a note that has a studied (non-new) card, or a
//              note tagged _card-status::i+0-manually
//   ankimorphs / file - its dictionary form (or UniDic lemma) is an AnkiMorphs-known lemma, or
//              listed in the known-morphs folder (snapshot at import)
//   learned  - it was known at some point (the learned table keeps every
//              known word for good, so the database stands without Anki)

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

const manualKnownTag = "_card-status::i+0-manually"

// studiedNotesSQL = notes whose words count as known through review.
const studiedNotesSQL = `SELECT note_id FROM cards WHERE type != 0
   UNION SELECT note_id FROM note_tags WHERE tag = '` + manualKnownTag + `'`

type knownState struct {
	known bool
	src   string
}

// deriveKnown computes the known status for the given morphs (nil = all).
func deriveKnown(db *sql.DB, ids []int64) (map[int64]knownState, error) {
	out := map[int64]knownState{}
	filter := ""
	if ids != nil {
		if len(ids) == 0 {
			return out, nil
		}
		filter = " WHERE m.id IN (" + joinIDs(ids) + ")"
	}
	rows, err := db.Query(`SELECT m.id, m.base, m.reading,
	   (SELECT known FROM manual_known mk WHERE mk.base=m.base AND mk.reading=m.reading),
	   EXISTS(SELECT 1 FROM note_morphs nm WHERE nm.morph_id=m.id AND nm.note_id IN (` + studiedNotesSQL + `)),
	   COALESCE((SELECT src FROM am_known a WHERE a.lemma=m.base), (SELECT src FROM am_known a WHERE a.lemma=m.lemma)),
	   (SELECT src FROM learned l WHERE l.base=m.base AND l.reading=m.reading)
	   FROM morphs m` + filter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var base, reading string
		var manual sql.NullInt64
		var review bool
		var am, learned sql.NullString
		if err := rows.Scan(&id, &base, &reading, &manual, &review, &am, &learned); err != nil {
			return nil, err
		}
		switch {
		case manual.Valid:
			out[id] = knownState{manual.Int64 == 1, "manual"}
		case review:
			out[id] = knownState{true, "review"}
		case learned.Valid:
			out[id] = knownState{true, learned.String}
		case am.Valid:
			src := am.String
			if strings.HasPrefix(src, "file:") {
				src = "file"
			}
			out[id] = knownState{true, src}
		default:
			out[id] = knownState{false, ""}
		}
	}
	return out, rows.Err()
}

// refreshKnown re-derives the given morphs (nil = all) and writes changes.
// Returns the ids whose known flag flipped.
func refreshKnown(db *sql.DB, ids []int64) ([]int64, error) {
	st, err := deriveKnown(db, ids)
	if err != nil {
		return nil, err
	}
	cur := map[int64]knownState{}
	q := `SELECT id, known, COALESCE(known_src,'') FROM morphs`
	if ids != nil {
		q += ` WHERE id IN (` + joinIDs(ids) + `)`
	}
	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var k bool
		var s string
		rows.Scan(&id, &k, &s)
		cur[id] = knownState{k, s}
	}
	rows.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	upd, _ := tx.Prepare(`UPDATE morphs SET known=?, known_src=NULLIF(?, '') WHERE id=?`)
	// every known word is saved for good: it stays known even if the card that
	// made it known leaves Anki (or Anki is replaced)
	learn, _ := tx.Prepare(`INSERT OR IGNORE INTO learned(base, reading, src, at) SELECT base, reading, ?, ? FROM morphs WHERE id=?`)
	now := time.Now().Unix()
	var flipped []int64
	for id, s := range st {
		if s.known && s.src != "manual" {
			learn.Exec(s.src, now, id)
		}
		c := cur[id]
		if c != s {
			upd.Exec(s.known, s.src, id)
			if c.known != s.known {
				flipped = append(flipped, id)
			}
		}
	}
	return flipped, tx.Commit()
}

// refreshNotes recounts i+N for notes containing any of the morphs (nil = all notes).
func refreshNotes(db *sql.DB, morphIDs []int64) (int, error) {
	where, nwhere := "", ""
	if morphIDs != nil {
		if len(morphIDs) == 0 {
			return 0, nil
		}
		sub := `SELECT DISTINCT note_id FROM note_morphs WHERE morph_id IN (` + joinIDs(morphIDs) + `)`
		where = ` WHERE nm.note_id IN (` + sub + `)`
		nwhere = ` AND id IN (` + sub + `)`
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmts := []string{
		`DROP TABLE IF EXISTS temp.agg`,
		`CREATE TEMP TABLE agg AS SELECT nm.note_id AS note_id, SUM(m.known = 0) AS nu,
		   MAX(CASE WHEN m.known = 0 THEN m.id END) AS um
		   FROM note_morphs nm JOIN morphs m ON m.id = nm.morph_id` + where + ` GROUP BY nm.note_id`,
		`UPDATE notes SET n_unknown = agg.nu, unk_morph = CASE WHEN agg.nu = 1 THEN agg.um END
		   FROM agg WHERE agg.note_id = notes.id AND (notes.n_unknown != agg.nu OR notes.unk_morph IS NOT (CASE WHEN agg.nu = 1 THEN agg.um END))`,
		// notes with no words at all
		`UPDATE notes SET n_unknown = 0, unk_morph = NULL WHERE n_morphs = 0 AND (n_unknown != 0 OR unk_morph IS NOT NULL)` + nwhere,
	}
	n := 0
	for _, s := range stmts {
		res, err := tx.Exec(s)
		if err != nil {
			return 0, fmt.Errorf("%w\n%s", err, s)
		}
		if strings.HasPrefix(strings.TrimSpace(s), "UPDATE") {
			k, _ := res.RowsAffected()
			n += int(k)
		}
	}
	tx.Exec(`DROP TABLE IF EXISTS temp.agg`)
	return n, tx.Commit()
}

// refreshPriority rebuilds the frequency-based priority list: every word's
// occurrence count over the parsed text of all cards, ranked.
func refreshPriority(db *sql.DB) error {
	t0 := time.Now()
	for _, s := range []string{
		`DROP TABLE IF EXISTS temp.f`,
		`CREATE TEMP TABLE f AS SELECT morph_id, SUM(cnt) AS c FROM note_morphs GROUP BY morph_id`,
		`UPDATE morphs SET freq = 0, rank = NULL`,
		`UPDATE morphs SET freq = f.c FROM f WHERE f.morph_id = morphs.id`,
		`WITH r AS (SELECT id, ROW_NUMBER() OVER (ORDER BY freq DESC, id) AS rn FROM morphs WHERE freq > 0)
		   UPDATE morphs SET rank = r.rn FROM r WHERE r.id = morphs.id`,
		`DROP TABLE IF EXISTS temp.f`,
	} {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%w\n%s", err, s)
		}
	}
	setMeta(db, "priority_built", time.Now().Format(time.RFC3339))
	log.Printf("priority list rebuilt in %s", time.Since(t0).Round(time.Millisecond))
	return nil
}

// recalcAll is the full rebuild (after import/parse). Day-to-day updates go
// through the incremental path in sync.go.
func recalcAll(db *sql.DB) error {
	t0 := time.Now()
	if err := refreshPriority(db); err != nil {
		return err
	}
	flipped, err := refreshKnown(db, nil)
	if err != nil {
		return err
	}
	log.Printf("known: derived (%d changed) in %s", len(flipped), time.Since(t0).Round(time.Millisecond))
	t1 := time.Now()
	n, err := refreshNotes(db, nil)
	if err != nil {
		return err
	}
	log.Printf("i+N: %d notes updated in %s", n, time.Since(t1).Round(time.Millisecond))
	if err := computeTagsets(db); err != nil {
		return err
	}
	return nil
}

// morphsOfNotes returns the distinct morph ids on the given notes.
func morphsOfNotes(db *sql.DB, noteIDs []int64) []int64 {
	if len(noteIDs) == 0 {
		return nil
	}
	rows, err := db.Query(`SELECT DISTINCT morph_id FROM note_morphs WHERE note_id IN (` + joinIDs(noteIDs) + `)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var m int64
		rows.Scan(&m)
		out = append(out, m)
	}
	return out
}

func joinIDs(ids []int64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d", id)
	}
	return b.String()
}
