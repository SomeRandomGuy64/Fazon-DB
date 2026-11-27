package main

// Lazy tag sets. A tag set is only recomputed when
//   (a) something uses it: the Tag sets tab, a tag: search, mining by tag
//       (alternates / prepare), or `mdb tagsets` — a note's tag list is
//       display only and never triggers a rebuild, and
//   (b) one of its inputs actually changed since it was last built.
//
// Inputs are tracked by SQLite triggers, so every writer counts (this binary,
// Autodeck's run_video.py, sqlite3 by hand):
//   - a note entering, leaving or changing inside the i+1 index -> its deck
//   - mined-word inputs (non-pool notes' Word, their cards turning studied,
//     the mined table), suspended words, deck renames               -> every set
//   - a set's own config or target list                              -> that set
// Deck-level marks land in tagset_stale_decks; staleTagsets() turns them into
// per-set generation bumps (dirty_gen) by matching each set's pool globs.
// A set is fresh when built_gen == dirty_gen.

import (
	"database/sql"
	"strings"
	"sync"
)

const allDecks = -1 // tagset_stale_decks sentinel: every set is stale

// lazyTagsetVersion: bump when a trigger below changes, so existing databases
// drop and re-create them (CREATE TRIGGER IF NOT EXISTS never replaces one).
// v2: UPDATE triggers fire only when a value really changes — the sync's
// per-minute deck upsert had been marking every set stale.
const lazyTagsetVersion = "2"

const lazyTagsetSchema = `
CREATE TABLE IF NOT EXISTS tagset_stale_decks(deck_id INTEGER PRIMARY KEY);

CREATE TRIGGER IF NOT EXISTS ts_note_upd AFTER UPDATE OF n_unknown, unk_morph, sentence, clen, deck_id ON notes
WHEN (OLD.n_unknown = 1 OR NEW.n_unknown = 1)
  AND (OLD.n_unknown IS NOT NEW.n_unknown OR OLD.unk_morph IS NOT NEW.unk_morph OR OLD.sentence IS NOT NEW.sentence
       OR OLD.clen IS NOT NEW.clen OR OLD.deck_id IS NOT NEW.deck_id)
BEGIN
  INSERT OR IGNORE INTO tagset_stale_decks VALUES(COALESCE(OLD.deck_id, 0));
  INSERT OR IGNORE INTO tagset_stale_decks VALUES(COALESCE(NEW.deck_id, 0));
END;
CREATE TRIGGER IF NOT EXISTS ts_note_del AFTER DELETE ON notes WHEN OLD.n_unknown = 1
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(COALESCE(OLD.deck_id, 0)); END;

CREATE TRIGGER IF NOT EXISTS ts_mined_note_ins AFTER INSERT ON notes
WHEN COALESCE(NEW.word, '') != '' AND NOT EXISTS (SELECT 1 FROM decks d WHERE d.id = NEW.deck_id AND d.is_pool = 1)
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_mined_note_upd AFTER UPDATE OF word, deck_id ON notes
WHEN (OLD.word IS NOT NEW.word OR OLD.deck_id IS NOT NEW.deck_id)
  AND NOT (EXISTS (SELECT 1 FROM decks d WHERE d.id = OLD.deck_id AND d.is_pool = 1)
       AND EXISTS (SELECT 1 FROM decks d WHERE d.id = NEW.deck_id AND d.is_pool = 1))
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_mined_note_del AFTER DELETE ON notes
WHEN COALESCE(OLD.word, '') != '' AND NOT EXISTS (SELECT 1 FROM decks d WHERE d.id = OLD.deck_id AND d.is_pool = 1)
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;

CREATE TRIGGER IF NOT EXISTS ts_card_upd AFTER UPDATE OF type ON cards
WHEN (COALESCE(OLD.type, 0) != 0) != (COALESCE(NEW.type, 0) != 0)
  AND EXISTS (SELECT 1 FROM notes n JOIN decks d ON d.id = n.deck_id WHERE n.id = NEW.note_id AND d.is_pool = 0 AND COALESCE(n.word, '') != '')
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_card_ins AFTER INSERT ON cards
WHEN COALESCE(NEW.type, 0) != 0
  AND EXISTS (SELECT 1 FROM notes n JOIN decks d ON d.id = n.deck_id WHERE n.id = NEW.note_id AND d.is_pool = 0 AND COALESCE(n.word, '') != '')
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_card_del AFTER DELETE ON cards
WHEN COALESCE(OLD.type, 0) != 0
  AND EXISTS (SELECT 1 FROM notes n JOIN decks d ON d.id = n.deck_id WHERE n.id = OLD.note_id AND d.is_pool = 0 AND COALESCE(n.word, '') != '')
BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;

CREATE TRIGGER IF NOT EXISTS ts_mined_ins AFTER INSERT ON mined BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_mined_del AFTER DELETE ON mined BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_susp_ins AFTER INSERT ON suspended_words BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_susp_del AFTER DELETE ON suspended_words BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_susp_upd AFTER UPDATE ON suspended_words
WHEN OLD.morph_id IS NOT NEW.morph_id OR OLD.note_id IS NOT NEW.note_id OR OLD.word IS NOT NEW.word BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_smark_ins AFTER INSERT ON suspend_marks BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_smark_del AFTER DELETE ON suspend_marks BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_smark_upd AFTER UPDATE ON suspend_marks
WHEN OLD.base IS NOT NEW.base OR OLD.reading IS NOT NEW.reading OR OLD.allowed IS NOT NEW.allowed BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;
CREATE TRIGGER IF NOT EXISTS ts_deck_upd AFTER UPDATE OF name, is_pool ON decks
WHEN OLD.name IS NOT NEW.name OR OLD.is_pool IS NOT NEW.is_pool BEGIN INSERT OR IGNORE INTO tagset_stale_decks VALUES(-1); END;

CREATE TRIGGER IF NOT EXISTS ts_cfg_upd AFTER UPDATE OF prefix, pool, creators, zatsudan, cap, ideal_len, min_len, max_len, enabled ON tagsets
WHEN OLD.prefix IS NOT NEW.prefix OR OLD.pool IS NOT NEW.pool OR OLD.creators IS NOT NEW.creators OR OLD.zatsudan IS NOT NEW.zatsudan
  OR OLD.cap IS NOT NEW.cap OR OLD.ideal_len IS NOT NEW.ideal_len OR OLD.min_len IS NOT NEW.min_len OR OLD.max_len IS NOT NEW.max_len
  OR OLD.enabled IS NOT NEW.enabled
BEGIN UPDATE tagsets SET dirty_gen = dirty_gen + 1 WHERE id = NEW.id; END;
CREATE TRIGGER IF NOT EXISTS ts_tgt_ins AFTER INSERT ON tagset_targets BEGIN UPDATE tagsets SET dirty_gen = dirty_gen + 1 WHERE id = NEW.tagset_id; END;
CREATE TRIGGER IF NOT EXISTS ts_tgt_del AFTER DELETE ON tagset_targets BEGIN UPDATE tagsets SET dirty_gen = dirty_gen + 1 WHERE id = OLD.tagset_id; END;
CREATE TRIGGER IF NOT EXISTS ts_tgt_upd AFTER UPDATE ON tagset_targets
WHEN OLD.tagset_id IS NOT NEW.tagset_id OR OLD.rank IS NOT NEW.rank OR OLD.word IS NOT NEW.word BEGIN UPDATE tagsets SET dirty_gen = dirty_gen + 1 WHERE id IN (OLD.tagset_id, NEW.tagset_id); END;
`

// migrateLazyTagsets adds the generation columns and the staleness triggers.
// New columns start stale (dirty 1, built 0), so each set rebuilds once on first use.
func migrateLazyTagsets(db *sql.DB) error {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tagsets') WHERE name='dirty_gen'`).Scan(&n)
	if n == 0 {
		for _, q := range []string{
			`ALTER TABLE tagsets ADD COLUMN dirty_gen INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE tagsets ADD COLUMN built_gen INTEGER NOT NULL DEFAULT 0`,
		} {
			if _, err := db.Exec(q); err != nil {
				return err
			}
		}
	}
	if getMeta(db, "lazy_tagset_schema") == lazyTagsetVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type='trigger' AND name GLOB 'ts_*'`)
	if err != nil {
		return err
	}
	var old []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		old = append(old, n)
	}
	rows.Close()
	for _, n := range old {
		if _, err := tx.Exec(`DROP TRIGGER "` + n + `"`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(lazyTagsetSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(k, v) VALUES('lazy_tagset_schema', ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, lazyTagsetVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// tagsetMu serialises rebuilds inside this process (HTTP handlers can ask at once).
var tagsetMu sync.Mutex

// staleTagsets folds the deck-level marks into per-set generation bumps and
// returns the ids of the sets that are stale now.
func staleTagsets(db *sql.DB) (map[int64]bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT deck_id FROM tagset_stale_decks`)
	if err != nil {
		return nil, err
	}
	var marks []int64
	all := false
	for rows.Next() {
		var d int64
		rows.Scan(&d)
		marks = append(marks, d)
		if d == allDecks {
			all = true
		}
	}
	rows.Close()
	if len(marks) > 0 {
		if all {
			if _, err := tx.Exec(`UPDATE tagsets SET dirty_gen = dirty_gen + 1`); err != nil {
				return nil, err
			}
		} else {
			names := map[string]bool{}
			r, err := tx.Query(`SELECT name FROM decks WHERE id IN (` + joinIDs(marks) + `)`)
			if err != nil {
				return nil, err
			}
			for r.Next() {
				var s string
				r.Scan(&s)
				names[s] = true
			}
			r.Close()
			sets, err := loadTagsetHeads(tx)
			if err != nil {
				return nil, err
			}
			var hit []int64
			for _, ts := range sets {
				for name := range names {
					if matchAny(ts.pool, name) {
						hit = append(hit, ts.id)
						break
					}
				}
			}
			if len(hit) > 0 {
				if _, err := tx.Exec(`UPDATE tagsets SET dirty_gen = dirty_gen + 1 WHERE id IN (` + joinIDs(hit) + `)`); err != nil {
					return nil, err
				}
			}
		}
		// only the marks read above: a concurrent writer's new marks survive
		if _, err := tx.Exec(`DELETE FROM tagset_stale_decks WHERE deck_id IN (` + joinIDs(marks) + `)`); err != nil {
			return nil, err
		}
	}
	out := map[int64]bool{}
	r, err := tx.Query(`SELECT id FROM tagsets WHERE dirty_gen != built_gen`)
	if err != nil {
		return nil, err
	}
	for r.Next() {
		var id int64
		r.Scan(&id)
		out[id] = true
	}
	r.Close()
	return out, tx.Commit()
}

// loadTagsetHeads = id + pool of every set (no targets), for deck matching.
func loadTagsetHeads(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]*tagset, error) {
	rows, err := q.Query(`SELECT id, prefix, pool FROM tagsets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*tagset
	for rows.Next() {
		t := &tagset{}
		var pool string
		rows.Scan(&t.id, &t.prefix, &pool)
		t.pool = parsePool(pool)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ensureTagsets rebuilds the stale sets that want() selects (nil = every set).
func ensureTagsets(db *sql.DB, want func(prefix string) bool) error {
	tagsetMu.Lock()
	defer tagsetMu.Unlock()
	stale, err := staleTagsets(db)
	if err != nil || len(stale) == 0 {
		return err
	}
	sets, err := loadTagsets(db)
	if err != nil {
		return err
	}
	var todo []*tagset
	for _, ts := range sets {
		if stale[ts.id] && (want == nil || want(ts.prefix)) {
			todo = append(todo, ts)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	return buildTagsets(db, todo)
}

// ensureTagsetsForTag: the sets a tag (or tag glob, `!higuchi-top2000::0123::01`, `!core*`) can belong to.
func ensureTagsetsForTag(db *sql.DB, tag string) error {
	head, _, _ := strings.Cut(strings.TrimSpace(tag), "::")
	re := deckGlob(head) // same * semantics; "(::.*)?" suffix is harmless for prefixes
	return ensureTagsets(db, func(p string) bool { return re.MatchString(p) })
}

// forceTagsets marks every set stale and rebuilds them all (`mdb tagsets`).
func forceTagsets(db *sql.DB) error {
	if _, err := db.Exec(`UPDATE tagsets SET dirty_gen = dirty_gen + 1`); err != nil {
		return err
	}
	return ensureTagsets(db, nil)
}

