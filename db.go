package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// Paths. Everything lives next to the binary's project folder unless overridden.
var (
	projectDir  = envOr("MDB_DIR", filepath.Join(os.Getenv("HOME"), "Language Learning", "mining-db"))
	dbPath      = envOr("MDB_DB", filepath.Join(projectDir, "data", "mining.db"))
	ankiProfile = envOr("MDB_ANKI_PROFILE", filepath.Join(os.Getenv("HOME"), ".local/share/Anki2/User 1"))
	addonsDir   = filepath.Join(filepath.Dir(ankiProfile), "addons21")
	// mining-db's own media store (hard links into the Anki folders, so no
	// extra disk) comes first; the Anki folders are fallbacks
	mediaStore = envOr("MDB_MEDIA", filepath.Join(projectDir, "media"))
	mediaDirs  = []string{
		mediaStore,
		filepath.Join(ankiProfile, "collection.media"),
		filepath.Join(addonsDir, "MvJ Japanese/user_files/database.media"),
	}
	parserDir   = envOr("MDB_PARSER_DIR", filepath.Join(os.Getenv("HOME"), "Language Learning", "jev_parser_test"))
	modelDir    = filepath.Join(parserDir, "models", "best")
	workerPy    = envOr("MDB_WORKER", filepath.Join(projectDir, "parser", "parse_worker.py"))
	mvjNotetype = "🇯🇵 MvJ"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);

CREATE TABLE IF NOT EXISTS decks(
  id INTEGER PRIMARY KEY,           -- Anki deck id
  name TEXT NOT NULL,               -- full name with :: separators
  is_pool INTEGER NOT NULL DEFAULT 0 -- 1 = subs2srs mining pool (Database::*)
);
CREATE INDEX IF NOT EXISTS decks_name ON decks(name);

-- MvJ per-note context store, split: the episode summary is shared by every
-- line of a video, so it is stored once.
CREATE TABLE IF NOT EXISTS summaries(id INTEGER PRIMARY KEY, text TEXT NOT NULL UNIQUE);

CREATE TABLE IF NOT EXISTS notes(
  id INTEGER PRIMARY KEY,            -- Anki note id
  guid TEXT,
  deck_id INTEGER,
  mod INTEGER,                       -- Anki note mod time
  in_anki INTEGER NOT NULL DEFAULT 1,
  sentence TEXT, sentence_audio TEXT, word TEXT, word_audio TEXT,
  definition TEXT, definition_audio TEXT, am_study_morphs TEXT, image TEXT,
  notes TEXT,                        -- MvJ "Notes" field (English translation)
  context TEXT,                      -- MvJ "Context" field as stored in Anki (usually empty)
  ctx_summary_id INTEGER,            -- mvj_context.db: episode summary
  ctx_lines TEXT,                    -- mvj_context.db: surrounding lines
  parse_field TEXT NOT NULL DEFAULT 'Sentence',
  -- parse + derived state
  parsed_model TEXT,                 -- fingerprint of the parser model used
  parsed_text TEXT,                  -- the text that was parsed (to detect edits)
  tokens TEXT,                       -- JSON [[surface, morph_id|0], ...]
  n_morphs INTEGER NOT NULL DEFAULT 0,
  n_unknown INTEGER NOT NULL DEFAULT 0,
  unk_morph INTEGER,                 -- the unknown morph when n_unknown = 1
  clen INTEGER NOT NULL DEFAULT 0    -- content length (no punctuation)
);
CREATE INDEX IF NOT EXISTS notes_deck ON notes(deck_id);
CREATE INDEX IF NOT EXISTS notes_unk ON notes(unk_morph) WHERE n_unknown = 1;
CREATE INDEX IF NOT EXISTS notes_nunk ON notes(n_unknown);
CREATE INDEX IF NOT EXISTS notes_guid ON notes(guid);

CREATE TABLE IF NOT EXISTS cards(
  id INTEGER PRIMARY KEY, note_id INTEGER NOT NULL, deck_id INTEGER,
  type INTEGER, queue INTEGER, ivl INTEGER, stability REAL, mod INTEGER
);
CREATE INDEX IF NOT EXISTS cards_note ON cards(note_id);
CREATE INDEX IF NOT EXISTS cards_studied ON cards(note_id) WHERE type != 0;

-- Imported Anki tags (everything except the ones this system now derives).
CREATE TABLE IF NOT EXISTS note_tags(note_id INTEGER NOT NULL, tag TEXT NOT NULL, PRIMARY KEY(note_id, tag)) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS note_tags_tag ON note_tags(tag);

CREATE TABLE IF NOT EXISTS morphs(
  id INTEGER PRIMARY KEY,
  base TEXT NOT NULL,                -- dictionary form (UniDic orthBase)
  lemma TEXT,                        -- UniDic lemma (為る for する, 無い for ない), for matching word lists
  reading TEXT NOT NULL,             -- dictionary-form reading from the reading model
  pos TEXT,
  freq INTEGER NOT NULL DEFAULT 0,   -- occurrences over the sentence text of all cards
  rank INTEGER,                      -- 1 = most frequent (the priority list)
  known INTEGER NOT NULL DEFAULT 0,
  known_src TEXT,                    -- manual / review / ankimorphs / file
  UNIQUE(base, reading)
);
CREATE INDEX IF NOT EXISTS morphs_base ON morphs(base);
CREATE INDEX IF NOT EXISTS morphs_rank ON morphs(rank);

CREATE TABLE IF NOT EXISTS note_morphs(
  note_id INTEGER NOT NULL, morph_id INTEGER NOT NULL, cnt INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY(note_id, morph_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS note_morphs_morph ON note_morphs(morph_id, note_id);

-- Known-word sources that are not derived from reviews.
CREATE TABLE IF NOT EXISTS am_known(lemma TEXT PRIMARY KEY, src TEXT NOT NULL) WITHOUT ROWID; -- AnkiMorphs snapshot + known-morphs files (lemma-keyed)
-- Every word that has ever been known, with where it first came from.
CREATE TABLE IF NOT EXISTS learned(base TEXT NOT NULL, reading TEXT NOT NULL, src TEXT NOT NULL, at INTEGER NOT NULL,
  PRIMARY KEY(base, reading)) WITHOUT ROWID;
-- per-word override of the ignore rule (1 = ignore, 0 = never ignore)
CREATE TABLE IF NOT EXISTS ignore_marks(base TEXT NOT NULL, reading TEXT NOT NULL, ignored INTEGER NOT NULL, at INTEGER,
  PRIMARY KEY(base, reading)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS manual_known(base TEXT NOT NULL, reading TEXT NOT NULL, known INTEGER NOT NULL, at INTEGER,
  PRIMARY KEY(base, reading)) WITHOUT ROWID;

-- Mining tag sets (the old *_daily add-ons), recomputed on the fly.
CREATE TABLE IF NOT EXISTS tagsets(
  id INTEGER PRIMARY KEY,
  prefix TEXT NOT NULL UNIQUE,       -- e.g. !higuchi-top2000
  name TEXT,
  pool TEXT NOT NULL,                -- JSON list of deck globs
  creators TEXT NOT NULL,            -- JSON [[name, glob], ...] priority order
  zatsudan TEXT NOT NULL DEFAULT '', -- deck glob; guarantee one alternate from here
  cap INTEGER NOT NULL DEFAULT 8, ideal_len INTEGER NOT NULL DEFAULT 18,
  min_len INTEGER NOT NULL DEFAULT 8, max_len INTEGER NOT NULL DEFAULT 45,
  enabled INTEGER NOT NULL DEFAULT 1,
  source TEXT                        -- where it was imported from
);
CREATE TABLE IF NOT EXISTS tagset_targets(tagset_id INTEGER NOT NULL, rank INTEGER NOT NULL, word TEXT NOT NULL,
  PRIMARY KEY(tagset_id, rank, word)) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS tagset_assign(
  tagset_id INTEGER NOT NULL, rank INTEGER NOT NULL, pos INTEGER NOT NULL, note_id INTEGER NOT NULL,
  word TEXT NOT NULL, tag TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tagset_assign_tag ON tagset_assign(tag);
CREATE INDEX IF NOT EXISTS tagset_assign_note ON tagset_assign(note_id);
CREATE INDEX IF NOT EXISTS tagset_assign_set ON tagset_assign(tagset_id, rank, pos);

CREATE TABLE IF NOT EXISTS mined(note_id INTEGER NOT NULL, tag TEXT, word TEXT, at INTEGER NOT NULL);

-- pool note -> the study card made from it (when mining created a new note)
CREATE TABLE IF NOT EXISTS study_links(study_id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL, at INTEGER NOT NULL);
`

func openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=30000&_foreign_keys=off&_cache_size=-200000")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	// migrations for databases created by earlier versions
	var hasLemma int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('morphs') WHERE name='lemma'`).Scan(&hasLemma)
	if hasLemma == 0 {
		db.Exec(`ALTER TABLE morphs ADD COLUMN lemma TEXT`)
	}
	db.Exec(`CREATE INDEX IF NOT EXISTS morphs_lemma ON morphs(lemma)`)
	var hasIgnored int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('morphs') WHERE name='ignored'`).Scan(&hasIgnored)
	if hasIgnored == 0 {
		db.Exec(`ALTER TABLE morphs ADD COLUMN ignored INTEGER NOT NULL DEFAULT 0`)
	}
	return db, nil
}

func getMeta(db *sql.DB, k string) string {
	var v string
	db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	return v
}

func setMeta(db *sql.DB, k, v string) {
	db.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
}
