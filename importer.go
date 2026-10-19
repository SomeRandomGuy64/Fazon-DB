package main

import (
	"compress/zlib"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func init() {
	// Anki's collection declares indexes with a custom "unicase" collation.
	sql.Register("sqlite3_anki", &sqlite3.SQLiteDriver{
		ConnectHook: func(c *sqlite3.SQLiteConn) error {
			return c.RegisterCollation("unicase", func(a, b string) int {
				return strings.Compare(strings.ToLower(a), strings.ToLower(b))
			})
		},
	})
}

// openCollection opens Anki's collection read-only. immutable=1 lets us read
// while Anki holds its lock; Anki checkpoints its WAL on every op, so the main
// file is current.
func openCollection() (*sql.DB, error) {
	p := filepath.Join(ankiProfile, "collection.anki2")
	return sql.Open("sqlite3_anki", "file:"+p+"?mode=ro&immutable=1")
}

// derivedTag reports tags this system now computes itself, so they are not
// imported as static tags: AnkiMorphs card status and the tag-set namespaces.
func derivedTag(tag string, prefixes []string) bool {
	// i+0 / i+1 / i+≥2 are recomputed here; "i+0-manually" is a user action and is kept
	if strings.HasPrefix(tag, "_card-status::") && !strings.HasSuffix(tag, "-manually") {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(tag, p+"::") {
			return true
		}
	}
	return false
}

type mvjFields struct{ idx map[string]int }

func (f mvjFields) get(parts []string, name string) string {
	i, ok := f.idx[name]
	if !ok || i >= len(parts) {
		return ""
	}
	return parts[i]
}

func cmdImport(db *sql.DB) error {
	t0 := time.Now()
	col, err := openCollection()
	if err != nil {
		return err
	}
	defer col.Close()

	var mid int64
	if err := col.QueryRow(`SELECT id FROM notetypes WHERE name=?`, mvjNotetype).Scan(&mid); err != nil {
		return fmt.Errorf("notetype %s: %w", mvjNotetype, err)
	}
	f := mvjFields{idx: map[string]int{}}
	rows, err := col.Query(`SELECT ord, name FROM fields WHERE ntid=?`, mid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o int
		var n string
		rows.Scan(&o, &n)
		f.idx[n] = o
	}
	rows.Close()

	// tag sets first, so their namespaces are known when tags are imported
	if err := importTagsets(db); err != nil {
		log.Printf("tagsets: %v", err)
	}
	prefixes := tagsetPrefixes(db)

	// decks
	tx, _ := db.Begin()
	rows, err = col.Query(`SELECT id, name FROM decks`)
	if err != nil {
		return err
	}
	nd := 0
	for rows.Next() {
		var id int64
		var name string
		rows.Scan(&id, &name)
		name = strings.ReplaceAll(name, "\x1f", "::")
		pool := 0
		if strings.HasPrefix(name, "Database::") {
			pool = 1
		}
		tx.Exec(`INSERT INTO decks(id,name,is_pool) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name, is_pool=excluded.is_pool`, id, name, pool)
		nd++
	}
	rows.Close()
	tx.Commit()
	log.Printf("decks: %d", nd)

	// notes (+ deck of the first card)
	rows, err = col.Query(`SELECT n.id, n.guid, n.mod, n.tags, n.flds,
	   (SELECT did FROM cards c WHERE c.nid = n.id ORDER BY ord LIMIT 1)
	   FROM notes n WHERE n.mid = ?`, mid)
	if err != nil {
		return err
	}
	tx, _ = db.Begin()
	upsert, _ := tx.Prepare(`INSERT INTO notes(id,guid,deck_id,mod,in_anki,sentence,sentence_audio,word,word_audio,definition,
	   definition_audio,am_study_morphs,image,notes,context,parse_field) VALUES(?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?)
	   ON CONFLICT(id) DO UPDATE SET guid=excluded.guid, deck_id=excluded.deck_id, mod=excluded.mod, in_anki=1,
	   sentence=excluded.sentence, sentence_audio=excluded.sentence_audio, word=excluded.word, word_audio=excluded.word_audio,
	   definition=excluded.definition, definition_audio=excluded.definition_audio, am_study_morphs=excluded.am_study_morphs,
	   image=excluded.image, notes=excluded.notes, context=excluded.context, parse_field=excluded.parse_field`)
	delTags, _ := tx.Prepare(`DELETE FROM note_tags WHERE note_id=?`)
	insTag, _ := tx.Prepare(`INSERT OR IGNORE INTO note_tags(note_id, tag) VALUES(?,?)`)
	seen := map[int64]bool{}
	n := 0
	for rows.Next() {
		var id, mod int64
		var did sql.NullInt64
		var guid, tags, flds string
		if err := rows.Scan(&id, &guid, &mod, &tags, &flds, &did); err != nil {
			return err
		}
		p := strings.Split(flds, "\x1f")
		tagList := strings.Fields(tags)
		pf := "Sentence"
		for _, t := range tagList {
			if strings.EqualFold(t, "Kaishi") { // AnkiMorphs reads Word on Kaishi notes
				pf = "Word"
			}
		}
		if _, err := upsert.Exec(id, guid, did, mod, f.get(p, "Sentence"), f.get(p, "Sentence Audio"), f.get(p, "Word"),
			f.get(p, "Word Audio"), f.get(p, "Definition"), f.get(p, "Definition Audio"), f.get(p, "am-study-morphs"),
			f.get(p, "Image"), f.get(p, "Notes"), f.get(p, "Context"), pf); err != nil {
			return err
		}
		delTags.Exec(id)
		for _, t := range tagList {
			if !derivedTag(t, prefixes) {
				insTag.Exec(id, t)
			}
		}
		seen[id] = true
		n++
		if n%100000 == 0 {
			tx.Commit()
			log.Printf("notes: %d", n)
			tx, _ = db.Begin()
			upsert, _ = tx.Prepare(`INSERT INTO notes(id,guid,deck_id,mod,in_anki,sentence,sentence_audio,word,word_audio,definition,
	   definition_audio,am_study_morphs,image,notes,context,parse_field) VALUES(?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?)
	   ON CONFLICT(id) DO UPDATE SET guid=excluded.guid, deck_id=excluded.deck_id, mod=excluded.mod, in_anki=1,
	   sentence=excluded.sentence, sentence_audio=excluded.sentence_audio, word=excluded.word, word_audio=excluded.word_audio,
	   definition=excluded.definition, definition_audio=excluded.definition_audio, am_study_morphs=excluded.am_study_morphs,
	   image=excluded.image, notes=excluded.notes, context=excluded.context, parse_field=excluded.parse_field`)
			delTags, _ = tx.Prepare(`DELETE FROM note_tags WHERE note_id=?`)
			insTag, _ = tx.Prepare(`INSERT OR IGNORE INTO note_tags(note_id, tag) VALUES(?,?)`)
		}
	}
	rows.Close()
	tx.Commit()
	log.Printf("notes: %d imported", n)

	// notes that are gone from Anki stay here (the pool outlives Anki), flagged
	if err := markMissing(db, seen); err != nil {
		return err
	}

	if err := importCards(db, col, mid); err != nil {
		return err
	}
	if err := importContext(db); err != nil {
		log.Printf("context: %v", err)
	}
	if err := importAMKnown(db); err != nil {
		log.Printf("ankimorphs known: %v", err)
	}
	setMeta(db, "last_import", time.Now().Format(time.RFC3339))
	log.Printf("import done in %s", time.Since(t0).Round(time.Second))
	return nil
}

func markMissing(db *sql.DB, seen map[int64]bool) error {
	rows, err := db.Query(`SELECT id FROM notes WHERE in_anki=1`)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	tx, _ := db.Begin()
	for _, id := range gone {
		tx.Exec(`UPDATE notes SET in_anki=0 WHERE id=?`, id)
	}
	tx.Commit()
	if len(gone) > 0 {
		log.Printf("notes no longer in Anki (kept, flagged): %d", len(gone))
	}
	return nil
}

func importCards(db, col *sql.DB, mid int64) error {
	rows, err := col.Query(`SELECT c.id, c.nid, c.did, c.type, c.queue, c.ivl,
	   CASE WHEN c.data != '' AND json_valid(c.data) THEN COALESCE(json_extract(c.data,'$.s'),0.0) ELSE 0.0 END, c.mod
	   FROM cards c JOIN notes n ON n.id=c.nid WHERE n.mid=?`, mid)
	if err != nil {
		return err
	}
	defer rows.Close()
	tx, _ := db.Begin()
	tx.Exec(`DELETE FROM cards`)
	ins, _ := tx.Prepare(`INSERT INTO cards(id,note_id,deck_id,type,queue,ivl,stability,mod) VALUES(?,?,?,?,?,?,?,?)`)
	n := 0
	for rows.Next() {
		var id, nid, did, typ, queue, ivl, mod int64
		var stab float64
		rows.Scan(&id, &nid, &did, &typ, &queue, &ivl, &stab, &mod)
		ins.Exec(id, nid, did, typ, queue, ivl, stab, mod)
		n++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("cards: %d", n)
	return nil
}

// importContext pulls MvJ's external context store (zlib rows keyed by guid).
func importContext(db *sql.DB) error {
	ctx, err := sql.Open("sqlite3", "file:"+filepath.Join(ankiProfile, "mvj_context.db")+"?mode=ro")
	if err != nil {
		return err
	}
	defer ctx.Close()
	guidToID := map[string]int64{}
	rows, err := db.Query(`SELECT id, guid FROM notes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var g string
		rows.Scan(&id, &g)
		guidToID[g] = id
	}
	rows.Close()

	sums := map[string]int64{}
	rows, _ = db.Query(`SELECT id, text FROM summaries`)
	for rows.Next() {
		var id int64
		var t string
		rows.Scan(&id, &t)
		sums[t] = id
	}
	rows.Close()

	crow, err := ctx.Query(`SELECT guid, enc, payload FROM context`)
	if err != nil {
		return err
	}
	defer crow.Close()
	tx, _ := db.Begin()
	insSum, _ := tx.Prepare(`INSERT INTO summaries(text) VALUES(?)`)
	upd, _ := tx.Prepare(`UPDATE notes SET ctx_summary_id=?, ctx_lines=? WHERE id=?`)
	n, miss := 0, 0
	for crow.Next() {
		var guid string
		var enc int
		var payload []byte
		crow.Scan(&guid, &enc, &payload)
		id, ok := guidToID[guid]
		if !ok {
			miss++
			continue
		}
		text := string(payload)
		if enc == 1 {
			r, err := zlib.NewReader(bytes.NewReader(payload))
			if err != nil {
				continue
			}
			b, _ := io.ReadAll(r)
			r.Close()
			text = string(b)
		}
		summary, lines := splitContext(text)
		var sid any
		if summary != "" {
			s, ok := sums[summary]
			if !ok {
				res, err := insSum.Exec(summary)
				if err != nil {
					return err
				}
				s, _ = res.LastInsertId()
				sums[summary] = s
			}
			sid = s
		}
		upd.Exec(sid, lines, id)
		n++
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("context: %d notes (%d summaries); %d store rows belong to non-MvJ/deleted notes", n, len(sums), miss)
	return nil
}

// splitContext splits "Episode summary: X | Sentence context: Y".
func splitContext(t string) (summary, lines string) {
	const sep = " | Sentence context: "
	if i := strings.Index(t, sep); i >= 0 {
		return strings.TrimPrefix(t[:i], "Episode summary: "), t[i+len(sep):]
	}
	if strings.HasPrefix(t, "Sentence context: ") {
		return "", strings.TrimPrefix(t, "Sentence context: ")
	}
	if strings.HasPrefix(t, "Episode summary: ") {
		return strings.TrimPrefix(t, "Episode summary: "), ""
	}
	return "", t
}

// importAMKnown snapshots AnkiMorphs' known lemmas and the known-morphs folder.
// These are lemma strings in AnkiMorphs' (MeCab) space; a word here is known
// when its dictionary form matches one of them, whatever its reading.
func importAMKnown(db *sql.DB) error {
	am, err := sql.Open("sqlite3", "file:"+filepath.Join(ankiProfile, "ankimorphs.db")+"?mode=ro")
	if err != nil {
		return err
	}
	defer am.Close()
	rows, err := am.Query(`SELECT lemma FROM Morphs GROUP BY lemma HAVING MAX(highest_lemma_learning_interval) >= 1`)
	if err != nil {
		return err
	}
	tx, _ := db.Begin()
	tx.Exec(`DELETE FROM am_known`)
	ins, _ := tx.Prepare(`INSERT OR IGNORE INTO am_known(lemma, src) VALUES(?,?)`)
	n := 0
	for rows.Next() {
		var l string
		rows.Scan(&l)
		ins.Exec(l, "ankimorphs")
		n++
	}
	rows.Close()
	files, _ := filepath.Glob(filepath.Join(ankiProfile, "known-morphs", "*.csv"))
	nf := 0
	for _, fp := range files {
		for _, l := range readCSVColumn(fp, "Morph-Lemma") {
			if _, err := ins.Exec(l, "file:"+filepath.Base(fp)); err == nil {
				nf++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("ankimorphs known lemmas: %d (+ %d from known-morphs files)", n, nf)
	return nil
}
