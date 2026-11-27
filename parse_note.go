package main

import (
	"database/sql"
	"fmt"
)

// noteParseWindow supplies immediate same-deck neighbors as context without
// rewriting them. IDs follow the ordering used by the corpus reparse.
func noteParseWindow(db *sql.DB, id int64) ([]parseLine, string, string, error) {
	var deck int64
	var field, sentence, word string
	err := db.QueryRow(`SELECT COALESCE(deck_id,0),parse_field,COALESCE(sentence,''),COALESCE(word,'') FROM notes WHERE id=?`, id).Scan(&deck, &field, &sentence, &word)
	if err != nil {
		return nil, "", "", err
	}
	text := parseText(field, sentence, word)
	lines := []parseLine{}
	for _, side := range []struct{ op, order string }{{"<", "DESC"}, {">", "ASC"}} {
		var neighbor int64
		var f, s, w string
		query := `SELECT id,parse_field,COALESCE(sentence,''),COALESCE(word,'') FROM notes WHERE COALESCE(deck_id,0)=? AND id ` + side.op + ` ? ORDER BY id ` + side.order + ` LIMIT 1`
		err := db.QueryRow(query, deck, id).Scan(&neighbor, &f, &s, &w)
		if err != nil && err != sql.ErrNoRows {
			return nil, "", "", err
		}
		if side.op == ">" {
			lines = append(lines, parseLine{id, text})
		}
		if err == nil {
			lines = append(lines, parseLine{neighbor, parseText(f, s, w)})
		}
	}
	return lines, text, sentence, nil
}

func cmdParseNote(db *sql.DB, id int64) error {
	// Accept either a note ID or a card ID, matching how cards are referred to in UI.
	var nid int64
	err := db.QueryRow(`SELECT id FROM notes WHERE id=?`, id).Scan(&nid)
	if err == sql.ErrNoRows {
		err = db.QueryRow(`SELECT note_id FROM cards WHERE id=?`, id).Scan(&nid)
	}
	if err != nil {
		return fmt.Errorf("note/card %d: %w", id, err)
	}
	lines, text, sentence, err := noteParseWindow(db, nid)
	if err != nil {
		return err
	}
	wk, err := startWorker()
	if err != nil {
		return err
	}
	defer wk.close()
	res, err := wk.parse(lines)
	if err != nil {
		return err
	}
	selected := []parseResult{}
	for _, r := range res {
		if r.ID == nid {
			selected = append(selected, r)
		}
	}
	if len(selected) != 1 {
		return fmt.Errorf("parser returned %d results for note %d", len(selected), nid)
	}
	mc := loadMorphCache(db)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	_, err = storeResults(tx, mc, modelFingerprint(), map[int64]string{nid: text}, map[int64]string{nid: sentence}, selected)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := recalcAll(db); err != nil {
		return err
	}
	fmt.Printf("reparsed note %d with same-deck neighbor context\n", nid)
	return nil
}

// cmdParseSelected repairs a selected set of notes. Each target gets its own
// padded three-line context; only the center result is persisted.
func cmdParseSelected(db *sql.DB, ids []int64) error {
	if len(ids) == 0 {
		return fmt.Errorf("no note IDs supplied")
	}
	unique := []int64{}
	seen := map[int64]bool{}
	for _, id := range ids {
		if !seen[id] {
			unique = append(unique, id)
			seen[id] = true
		}
	}
	wk, err := startWorker()
	if err != nil {
		return err
	}
	defer wk.close()
	mc, fp := loadMorphCache(db), modelFingerprint()
	for start := 0; start < len(unique); start += 64 {
		end := start + 64
		if end > len(unique) {
			end = len(unique)
		}
		lines := []parseLine{}
		texts, sents := map[int64]string{}, map[int64]string{}
		for _, id := range unique[start:end] {
			window, text, sentence, err := noteParseWindow(db, id)
			if err != nil {
				return fmt.Errorf("note %d: %w", id, err)
			}
			var before, after parseLine
			for _, line := range window {
				if line.ID < id {
					before = line
				}
				if line.ID > id {
					after = line
				}
			}
			lines = append(lines, before, parseLine{id, text}, after)
			texts[id], sents[id] = text, sentence
		}
		results, err := wk.parse(lines)
		if err != nil {
			return err
		}
		if len(results) != len(lines) {
			return fmt.Errorf("expected %d parser results, got %d", len(lines), len(results))
		}
		selected := []parseResult{}
		for i, id := range unique[start:end] {
			center := results[3*i+1]
			if center.ID != id {
				return fmt.Errorf("parser result order mismatch for %d", id)
			}
			selected = append(selected, center)
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		_, err = storeResults(tx, mc, fp, texts, sents, selected)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		fmt.Printf("selected reparse: %d/%d notes committed\n", end, len(unique))
	}
	return recalcAll(db)
}
