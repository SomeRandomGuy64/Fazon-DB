package main

// Live sync with Anki over AnkiConnect, and the incremental recalc it drives.
// `mdb serve` runs syncOnce every minute, so a review in Anki shows up here
// (known words, i+N, tag sets) within a minute — no full recalc.

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// writeMu serialises every writer (sync loop, API edits, CLI in-process).
var writeMu sync.Mutex

type syncStats struct {
	notes, cards, reparsed, flipped, notesRecounted int
	tagsets                                         bool
}

func (s syncStats) String() string {
	return fmt.Sprintf("%d notes, %d cards changed; %d reparsed; %d words flipped known/unknown; %d notes recounted; tagsets=%v",
		s.notes, s.cards, s.reparsed, s.flipped, s.notesRecounted, s.tagsets)
}

// syncOnce pulls what changed in Anki since `days` ago (1 = today) and applies it.
// full=true also re-checks every studied card (catches resets/forgets).
func syncOnce(db *sql.DB, days int, full bool) (syncStats, error) {
	writeMu.Lock()
	defer writeMu.Unlock()
	var st syncStats

	// decks (new subs2srs decks, renames)
	var deckIDs map[string]int64
	if err := ac("deckNamesAndIds", nil, &deckIDs); err != nil {
		return st, err
	}
	tx, _ := db.Begin()
	for name, id := range deckIDs {
		pool := 0
		if strings.HasPrefix(name, "Database::") {
			pool = 1
		}
		tx.Exec(`INSERT INTO decks(id,name,is_pool) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name, is_pool=excluded.is_pool`, id, name, pool)
	}
	tx.Commit()

	recent := fmt.Sprintf(`"note:%s" (rated:%d OR edited:%d OR added:%d)`, mvjNotetype, days, days, days)

	// ---- notes: content/tags
	var nids []int64
	if err := ac("findNotes", map[string]any{"query": recent}, &nids); err != nil {
		return st, err
	}
	changedNotes, err := changedByMod(db, "notes", "notesModTime", "noteId", nids)
	if err != nil {
		return st, err
	}
	var touchedNotes []int64
	var toParse []int64
	err = chunked(changedNotes, 200, func(c []int64) error {
		var infos []acNote
		if err := ac("notesInfo", map[string]any{"notes": c}, &infos); err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		prefixes := tagsetPrefixes(db)
		for _, n := range infos {
			if n.NoteID == 0 {
				continue
			}
			fv := func(k string) string { return n.Fields[k].Value }
			pf := "Sentence"
			for _, t := range n.Tags {
				if strings.EqualFold(t, "Kaishi") {
					pf = "Word"
				}
			}
			var oldText string
			tx.QueryRow(`SELECT COALESCE(parsed_text,'') FROM notes WHERE id=?`, n.NoteID).Scan(&oldText)
			_, err := tx.Exec(`INSERT INTO notes(id,mod,in_anki,sentence,sentence_audio,word,word_audio,definition,definition_audio,
			   am_study_morphs,image,notes,context,parse_field) VALUES(?,?,1,?,?,?,?,?,?,?,?,?,?,?)
			   ON CONFLICT(id) DO UPDATE SET mod=excluded.mod, in_anki=1, sentence=excluded.sentence, sentence_audio=excluded.sentence_audio,
			   word=excluded.word, word_audio=excluded.word_audio, definition=excluded.definition, definition_audio=excluded.definition_audio,
			   am_study_morphs=excluded.am_study_morphs, image=excluded.image, notes=excluded.notes, context=excluded.context,
			   parse_field=excluded.parse_field`,
				n.NoteID, n.Mod, fv("Sentence"), fv("Sentence Audio"), fv("Word"), fv("Word Audio"), fv("Definition"),
				fv("Definition Audio"), fv("am-study-morphs"), fv("Image"), fv("Notes"), fv("Context"), pf)
			if err != nil {
				tx.Rollback()
				return err
			}
			tx.Exec(`DELETE FROM note_tags WHERE note_id=?`, n.NoteID)
			for _, t := range n.Tags {
				if !derivedTag(t, prefixes) {
					tx.Exec(`INSERT OR IGNORE INTO note_tags(note_id, tag) VALUES(?,?)`, n.NoteID, t)
				}
			}
			if parseText(pf, fv("Sentence"), fv("Word")) != oldText {
				toParse = append(toParse, n.NoteID)
			}
			touchedNotes = append(touchedNotes, n.NoteID)
			// a card id list lets us pick up cards of brand-new notes below
		}
		return tx.Commit()
	})
	if err != nil {
		return st, err
	}
	st.notes = len(changedNotes)

	// ---- cards: review state + deck
	var cids []int64
	q := recent
	if full {
		q = fmt.Sprintf(`"note:%s" (-is:new OR rated:%d OR edited:%d OR added:%d)`, mvjNotetype, days, days, days)
	}
	if err := ac("findCards", map[string]any{"query": q}, &cids); err != nil {
		return st, err
	}
	// suspending/unsuspending touches neither the note nor a review: check the
	// Study Deck's suspended cards (and the ones held as suspended) every pass
	var susp []int64
	if err := ac("findCards", map[string]any{"query": `"deck:` + studyDeck + `" is:suspended`}, &susp); err != nil {
		return st, err
	}
	cids = append(cids, susp...)
	if rows, err := db.Query(`SELECT id FROM cards WHERE queue = -1 AND deck_id IN (` + studyDeckSQL + `)`); err == nil {
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			cids = append(cids, id)
		}
		rows.Close()
	}
	changedCards, err := changedByMod(db, "cards", "cardsModTime", "cardId", cids)
	if err != nil {
		return st, err
	}
	err = chunked(changedCards, 200, func(c []int64) error {
		var infos []acCard
		if err := ac("cardsInfo", map[string]any{"cards": c}, &infos); err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		for _, cd := range infos {
			if cd.CardID == 0 {
				continue
			}
			did := deckIDs[cd.DeckName]
			tx.Exec(`INSERT INTO cards(id,note_id,deck_id,type,queue,ivl,mod) VALUES(?,?,?,?,?,?,?)
			   ON CONFLICT(id) DO UPDATE SET note_id=excluded.note_id, deck_id=excluded.deck_id, type=excluded.type,
			   queue=excluded.queue, ivl=excluded.ivl, mod=excluded.mod`, cd.CardID, cd.Note, did, cd.Type, cd.Queue, cd.Interval, cd.Mod)
			if cd.Ord == 0 {
				tx.Exec(`UPDATE notes SET deck_id=? WHERE id=?`, did, cd.Note)
			}
			touchedNotes = append(touchedNotes, cd.Note)
		}
		return tx.Commit()
	})
	if err != nil {
		return st, err
	}
	st.cards = len(changedCards)
	setMeta(db, "last_sync", time.Now().Format(time.RFC3339))
	if len(touchedNotes) == 0 {
		return st, nil
	}

	// ---- incremental recalc
	touchedMorphs := map[int64]bool{}
	if len(toParse) == 0 { // reparsed notes refresh it below, once their words are in
		ms, err := refreshSuspended(db)
		if err != nil {
			return st, err
		}
		for _, m := range ms {
			touchedMorphs[m] = true
		}
	}
	for _, m := range morphsOfNotes(db, touchedNotes) {
		touchedMorphs[m] = true
	}
	if len(toParse) > 0 {
		ms, err := parseNotes(db, toParse)
		if err != nil {
			return st, err
		}
		for m := range ms {
			touchedMorphs[m] = true
		}
		st.reparsed = len(toParse)
		sm, err := refreshSuspended(db)
		if err != nil {
			return st, err
		}
		for _, m := range sm {
			touchedMorphs[m] = true
		}
	}
	ids := keys(touchedMorphs)
	flipped, err := refreshKnown(db, ids)
	if err != nil {
		return st, err
	}
	st.flipped = len(flipped)
	// recount the flipped words' notes, plus the reparsed notes themselves
	recount := append(flipped, morphsOfNotes(db, toParse)...)
	n, err := refreshNotes(db, recount)
	if err != nil {
		return st, err
	}
	if len(toParse) > 0 {
		n2, _ := refreshNotesByID(db, toParse)
		n += n2
	}
	st.notesRecounted = n
	if st.flipped > 0 || st.reparsed > 0 || st.cards > 0 {
		if err := computeTagsets(db); err != nil {
			return st, err
		}
		st.tagsets = true
	}
	return st, nil
}

// refreshNotesByID recounts specific notes (e.g. ones whose words were all removed).
func refreshNotesByID(db *sql.DB, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res, err := db.Exec(`UPDATE notes SET
	   n_unknown = (SELECT COUNT(*) FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE nm.note_id=notes.id AND m.known=0 AND m.ignored=0),
	   unk_morph = CASE WHEN (SELECT COUNT(*) FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE nm.note_id=notes.id AND m.known=0 AND m.ignored=0) = 1
	     THEN (SELECT m.id FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE nm.note_id=notes.id AND m.known=0 AND m.ignored=0) END
	   WHERE id IN (` + joinIDs(ids) + `)`)
	if err != nil {
		return 0, err
	}
	k, _ := res.RowsAffected()
	return int(k), nil
}

// changedByMod returns the ids whose Anki mod time differs from what we hold.
func changedByMod(db *sql.DB, table, action, idKey string, ids []int64) ([]int64, error) {
	var out []int64
	err := chunked(ids, 1000, func(c []int64) error {
		var mods []map[string]int64
		param := "cards"
		if table == "notes" {
			param = "notes"
		}
		if err := ac(action, map[string]any{param: c}, &mods); err != nil {
			return err
		}
		have := map[int64]int64{}
		rows, err := db.Query(`SELECT id, COALESCE(mod,0) FROM ` + table + ` WHERE id IN (` + joinIDs(c) + `)`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, m int64
			rows.Scan(&id, &m)
			have[id] = m
		}
		rows.Close()
		for _, m := range mods {
			id := m[idKey]
			if hm, ok := have[id]; !ok || hm != m["mod"] {
				out = append(out, id)
			}
		}
		return nil
	})
	return out, err
}

// parseNotes (re)parses specific notes with a short-lived worker.
func parseNotes(db *sql.DB, ids []int64) (map[int64]bool, error) {
	fp := modelFingerprint()
	rows, err := db.Query(`SELECT id, parse_field, COALESCE(sentence,''), COALESCE(word,'') FROM notes WHERE id IN (` + joinIDs(ids) + `) ORDER BY deck_id, id`)
	if err != nil {
		return nil, err
	}
	var lines []parseLine
	texts, sents := map[int64]string{}, map[int64]string{}
	for rows.Next() {
		var id int64
		var f, s, w string
		rows.Scan(&id, &f, &s, &w)
		t := parseText(f, s, w)
		lines = append(lines, parseLine{id, t})
		texts[id], sents[id] = t, s
	}
	rows.Close()
	wk, err := sharedWorker()
	if err != nil {
		return nil, err
	}
	touched := map[int64]bool{}
	mc := loadMorphCache(db)
	for i := 0; i < len(lines); i += 384 {
		j := i + 384
		if j > len(lines) {
			j = len(lines)
		}
		res, err := wk.parse(lines[i:j])
		if err != nil {
			resetSharedWorker()
			return nil, err
		}
		tx, _ := db.Begin()
		t, err := storeResults(tx, mc, fp, texts, sents, res)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		for m := range t {
			touched[m] = true
		}
	}
	// brand-new words need a frequency rank
	db.Exec(`UPDATE morphs SET freq = (SELECT COALESCE(SUM(cnt),0) FROM note_morphs WHERE morph_id = morphs.id) WHERE id IN (` + joinIDs(keys(touched)) + `)`)
	return touched, nil
}

// One parser process kept warm for the server (model load takes ~5 s).
var (
	sharedMu sync.Mutex
	shared   *worker
	lastUse  time.Time
)

func sharedWorker() (*worker, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	lastUse = time.Now()
	if shared != nil {
		return shared, nil
	}
	w, err := startWorker()
	if err != nil {
		return nil, err
	}
	shared = w
	go func() { // free the GPU after 10 idle minutes
		for {
			time.Sleep(time.Minute)
			sharedMu.Lock()
			if shared == w && time.Since(lastUse) > 10*time.Minute {
				shared.close()
				shared = nil
				sharedMu.Unlock()
				return
			}
			sharedMu.Unlock()
		}
	}()
	return w, nil
}

func resetSharedWorker() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if shared != nil {
		shared.close()
		shared = nil
	}
}

func keys(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func syncLoop(db *sql.DB, every time.Duration) {
	first := true
	for {
		st, err := syncOnce(db, 1, first)
		if err != nil {
			log.Printf("sync: %v", err)
		} else if st.notes+st.cards > 0 {
			log.Printf("sync: %s", st)
		}
		first = false
		time.Sleep(every)
	}
}
