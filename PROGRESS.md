# mining-db — build progress (resume here if a session is cut off)

Request (2026-10-19): local SQLite mining DB (Go) replacing AnkiMorphs: all subs2srs
(Database::*) + mined MvJ notes with every field + MvJ context; Anki known words = known;
incremental i+N (no slow recalc); tag sets (*_daily add-ons) preserved and recomputed
on the fly; frequency priority list from all cards' sentence text; Hetzner backup; web
harness; /mvj-mine adapted to use the DB. Parser = Faizan's reading model
(jev_parser_test/models/best via reading_disambig) — Japanese only.

## Done
- [x] Go project, schema (db.go), `mdb import` (40 s for 1.02M notes; context store split
      into summaries + lines; AnkiMorphs known lemmas + known-morphs files snapshotted)
- [x] parser/parse_worker.py (JSONL; AnkiMorphs-equivalent filters: symbols, numerals,
      proper nouns, OOV, bracket contents)
- [x] engine.go incremental known → i+N; tagsets.go port of engine.py; sync.go AnkiConnect
      polling; mine.go (alternates/prepare/mined); backup.go; query.go
- [~] full parse running: `data/parse.log` (resumable: rerun `./mdb parse -j 3`)

## Todo
- [ ] web harness (server.go + web/)
- [ ] verify known counts / tag-set parity vs the Anki add-ons
- [ ] backup to Hetzner + systemd user timer; serve as systemd user service
- [ ] /mvj-mine: DB-backed precheck + bridge `mine` by nid
- [ ] memory note
