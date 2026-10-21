package main

// Anki-independent pieces: a portable export any SRS can read, importing
// known words from any app, and pruning the pool out of Anki once the
// database holds it (Anki then only keeps what you actually review).

import (
	"bufio"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cmdExport writes DIR/{notes.jsonl, known_words.csv, decks.csv, README.txt}.
// Media is not copied (it's ~170 GB); notes reference file names in
// Fazon-DB/media.
func cmdExport(db *sql.DB, dir, query string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	where, args, err := buildWhere(db, query)
	if err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "notes.jsonl"))
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	rows, err := db.Query(`SELECT n.id, COALESCE(n.guid,''), COALESCE(d.name,''), n.in_anki, COALESCE(n.sentence,''), COALESCE(n.sentence_audio,''),
	   COALESCE(n.word,''), COALESCE(n.word_audio,''), COALESCE(n.definition,''), COALESCE(n.definition_audio,''), COALESCE(n.image,''),
	   COALESCE(n.notes,''), COALESCE(s.text,''), COALESCE(n.ctx_lines,''), n.n_unknown, COALESCE(n.tokens,'[]')
	   FROM notes n LEFT JOIN decks d ON d.id=n.deck_id LEFT JOIN summaries s ON s.id=n.ctx_summary_id WHERE `+where+` ORDER BY n.deck_id, n.id`, args...)
	if err != nil {
		f.Close()
		return err
	}
	n := 0
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for rows.Next() {
		var id int64
		var inAnki, nu int
		var guid, deck, sent, sa, word, wa, def, da, img, notes, sum, lines, toks string
		rows.Scan(&id, &guid, &deck, &inAnki, &sent, &sa, &word, &wa, &def, &da, &img, &notes, &sum, &lines, &nu, &toks)
		var tk any
		json.Unmarshal([]byte(toks), &tk)
		enc.Encode(map[string]any{
			"id": id, "guid": guid, "deck": deck, "in_anki": inAnki == 1,
			"sentence": sent, "translation": notes, "word": word, "definition": def,
			"media": map[string]any{"sentence_audio": audioName(sa), "image": imgName(img), "word_audio": audioName(wa), "definition_audio": noteMedia(da)},
			"fields_raw": map[string]string{"Sentence Audio": sa, "Word Audio": wa, "Definition Audio": da, "Image": img},
			"context": map[string]string{"summary": sum, "lines": lines},
			"n_unknown": nu, "tokens": tk,
		})
		n++
	}
	rows.Close()
	w.Flush()
	f.Close()

	kf, err := os.Create(filepath.Join(dir, "known_words.csv"))
	if err != nil {
		return err
	}
	cw := csv.NewWriter(kf)
	cw.Write([]string{"base", "reading", "lemma", "source", "rank", "freq"})
	krows, err := db.Query(`SELECT base, reading, COALESCE(lemma,''), COALESCE(known_src,''), COALESCE(rank,0), freq FROM morphs WHERE known=1 ORDER BY rank`)
	if err != nil {
		return err
	}
	k := 0
	for krows.Next() {
		var b, r, l, s string
		var rank, freq int
		krows.Scan(&b, &r, &l, &s, &rank, &freq)
		cw.Write([]string{b, r, l, s, fmt.Sprint(rank), fmt.Sprint(freq)})
		k++
	}
	krows.Close()
	cw.Flush()
	kf.Close()
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte(fmt.Sprintf(`Fazon-DB export %s
notes.jsonl      one card per line: sentence, translation, word, definition, media file names,
                 context (episode summary + neighbouring lines), tokens [surface, word id], i+N
known_words.csv  every known word (dictionary form, reading, lemma, source, frequency rank)
Media files live in Fazon-DB/media/ under the names given in "media".
`, time.Now().Format(time.RFC3339))), 0o644)
	log.Printf("export → %s: %d notes, %d known words", dir, n, k)
	return nil
}

// cmdImportKnown marks words from any app's export as learned.
// Input: CSV/TSV/plain lines of  word[,reading]  (header optional).
func cmdImportKnown(db *sql.DB, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	src := "import:" + filepath.Base(path)
	now := time.Now().Unix()
	tx, _ := db.Begin()
	n := 0
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if line == "" {
			continue
		}
		parts := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' })
		base := strings.TrimSpace(parts[0])
		if i == 0 && (strings.EqualFold(base, "base") || strings.EqualFold(base, "word") || strings.EqualFold(base, "Morph-Lemma")) {
			continue
		}
		reading := ""
		if len(parts) > 1 {
			reading = strings.TrimSpace(parts[1])
		}
		var res sql.Result
		if reading != "" {
			res, err = tx.Exec(`INSERT OR IGNORE INTO learned(base, reading, src, at) VALUES(?,?,?,?)`, base, reading, src, now)
		} else { // every reading of the word (and words whose lemma it is)
			res, err = tx.Exec(`INSERT OR IGNORE INTO learned(base, reading, src, at)
			   SELECT base, reading, ?, ? FROM morphs WHERE base = ? OR lemma = ?`, src, now, base, base)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("import-known: %d new learned entries from %s", n, path)
	flipped, err := refreshKnown(db, nil)
	if err != nil {
		return err
	}
	if _, err := refreshNotes(db, flipped); err != nil {
		return err
	}
	return computeTagsets(db)
}

// cmdAnkiPrune deletes subs2srs pool notes from Anki once the database fully
// holds them: parsed, every media file present in Fazon-DB/media, and never
// studied. Dry run unless apply.
func cmdAnkiPrune(db *sql.DB, deckGlob string, apply bool) error {
	q := `SELECT n.id, COALESCE(n.sentence_audio,''), COALESCE(n.image,'') FROM notes n JOIN decks d ON d.id=n.deck_id
	   WHERE d.is_pool=1 AND n.in_anki=1 AND n.parsed_model IS NOT NULL
	   AND NOT EXISTS (SELECT 1 FROM cards c WHERE c.note_id=n.id AND c.type != 0)`
	var args []any
	if deckGlob != "" {
		ids, err := decksMatching(db, deckGlob)
		if err != nil {
			return err
		}
		q += ` AND n.deck_id IN (` + joinIDs(ids) + `)`
		if len(ids) == 0 {
			return fmt.Errorf("no deck matches %s", deckGlob)
		}
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	var ok []int64
	noMedia := 0
	for rows.Next() {
		var id int64
		var a, i string
		rows.Scan(&id, &a, &i)
		good := true
		for _, m := range noteMedia(a, i) {
			if _, err := os.Stat(filepath.Join(mediaStore, filepath.Base(m))); err != nil {
				good = false
			}
		}
		if good {
			ok = append(ok, id)
		} else {
			noMedia++
		}
	}
	rows.Close()
	log.Printf("anki-prune: %d pool notes can leave Anki; %d kept because their media is not in Fazon-DB/media yet (run media-adopt)", len(ok), noMedia)
	if !apply {
		log.Printf("dry run — nothing deleted. Re-run with -apply to delete them from Anki.")
		return nil
	}
	done := 0
	err = chunked(ok, 500, func(c []int64) error {
		if err := ac("deleteNotes", map[string]any{"notes": c}, nil); err != nil {
			return err
		}
		db.Exec(`UPDATE notes SET in_anki=0 WHERE id IN (` + joinIDs(c) + `)`)
		db.Exec(`DELETE FROM cards WHERE note_id IN (` + joinIDs(c) + `)`)
		done += len(c)
		if done%20000 == 0 {
			log.Printf("anki-prune: %d/%d", done, len(ok))
		}
		return nil
	})
	log.Printf("anki-prune: removed %d notes from Anki (kept in Fazon-DB)", done)
	return err
}
