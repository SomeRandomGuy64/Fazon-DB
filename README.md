# Fazon-DB

A local sentence-mining database that replaces AnkiMorphs. It holds every subs2srs
(`Database::*`) card and every mined MvJ card, with all fields and the MvJ context. Each card
is tokenized with **your reading model** (`jev_parser_test/models/best`: Sudachi Full C-mode segmentation,
ModernBERT homograph readings). Known words, i+N, the frequency priority list and the mining
tag sets are kept up to date incrementally, so there is never a full recalc.

```
./mdb serve          # harness on http://127.0.0.1:8790, live Anki sync every minute, daily backup
./mdb status         # counts, parser model freshness, last sync/backup
./mdb search 'i:1 deck:Database::higuchi_mengen* unk:守備'
```

## How it fits together

| Piece | Where |
|---|---|
| Schema | `db.go`. One SQLite file, `data/fazon.db` |
| Import from Anki | `importer.go`. Reads the collection read-only. Brings in notes, cards, tags (minus the derived ones), the MvJ context store (summary stored once per video), the AnkiMorphs known-lemma snapshot and the known-morphs files |
| Parser | `parser/parse_worker.py`. JSONL worker around `reading_disambig`. Filters match your AnkiMorphs settings: punctuation, numerals, proper nouns, out-of-dictionary tokens, bracket contents |
| Known words | `engine.go`. A word is known if marked known here, or it's on a studied card or an `i+0-manually` card, or its dictionary form/lemma is AnkiMorphs-known |
| i+N | `engine.go`. Only notes containing a word whose status flipped are recounted |
| Priority | Word frequency over the parsed text of all your cards (`morphs.rank`) |
| Tag sets | `tagsets.go` + `lazytagsets.go`. Go port of the `*_daily` add-ons' engine, imported from the add-on folders. Lazy: SQLite triggers mark a set stale only when its inputs change (i+1 notes in its pool decks, mined/studied words, suspended words, its own targets/config); it is rebuilt (~1–2 s) only when used — Tag sets tab, `tag:` search, mining by tag, `mdb tagsets`. `/api/status` reports `stale` per set |
| Live sync | `sync.go`. AnkiConnect polling (reviews, edits, new decks), then the incremental recalc |
| Harness | `server.go` + `web/index.html`. Overview, cards, words (mark known/unknown), tag sets, card detail with per-token readings + romaji, audio/images from collection.media / database.media |
| Backup | `backup.go`. `VACUUM INTO` snapshot, zstd, scp to `root@89.167.79.200:/var/backups/fazon-db`. Keeps 14 there and 3 locally |
| Mining | `mine.go`. `alternates` / `prepare` / `mined` for the `/mvj-mine` skill |

## Long-term: the database first, Anki only for reviews

- New subs2srs deck: `./mdb import-apkg path/to/deck.apkg`. It never touches Anki. Media goes
  into `Fazon-DB/media/`, the cards are tokenized right away and join the tag-set pools.
- Mining (`/mvj-mine`): `mdb prepare` creates the card straight in Study Deck when its note only
  lives in the database. Known words are saved in the `learned` table for good.
- `./mdb anki-prune` (dry run) / `-apply` removes pool notes from Anki once the database fully
  holds them (parsed, media in `media/`, never studied).
- Moving to another SRS: `./mdb export DIR` writes `notes.jsonl` and `known_words.csv`.
  `./mdb import-known FILE` reads known words back from any app. `sync.go` is the only
  Anki-specific part to replace.

## Sudachi parser migration (2026-11-04)

The worker uses the shared `reading_disambig` Sudachi adapter, including separate
surface and dictionary-form readings, preserved A/B alternatives, and the existing
main + variant reading models. Its card and live-furigana JSON row formats remain
compatible. Existing symbol/numeral/proper-noun/OOV filters remain in the worker.
An explicit fallback is available with `MDB_PARSER_BACKEND=unidic` (or the worker's
`--backend unidic`); it retains the previous UniDic merge repairs.

The expanded reading gate also checks common readings against a conflicting
analyzer baseline without requiring two separate common JMdict entries. Existing
model targets retain their candidate lists. `--legacy-gate` on the Python worker
restores the earlier gate for diagnosis. See [expanded gate notes](../../jev_parser_test/EXPANDED_GATE.md).
Numeric dates/counters are protected from newly expanded overrides when the
analyzer already supplies a reading. See [counter protection](../../jev_parser_test/COUNTER_GUARD.md).
`./mdb parse-selected FILE` repairs newline-separated note IDs with independent
same-deck neighbor contexts and one derived-count rebuild.
`./mdb parse-note ID` repairs one stored note/card with same-deck neighbor context
and rebuilds the derived database counts.

Parser freshness now includes adapter/shared-code changes, installed Sudachi
and dictionary versions, backend selection, and both model configurations. A
migration requires `./mdb parse` to refresh old notes and recompute frequencies,
known status, i+N, and tag sets. Manual knowledge marks are preserved.

Validation and limitations: [migration report](../../jev_parser_test/SUDACHI_MIGRATION_REPORT.md).

## Common jobs

- **Improved the model?** Replace `jev_parser_test/models/best`, then run `./mdb parse` (or
  `./mdb reparse` to force it). The model fingerprint changes, so every card is re-tokenized
  (~14 min on the GPU with 2 workers; each holds the main + variant model, ~3 GB, so 3 don't fit in 12 GB). Known status is re-derived. Only manual marks are keyed
  by reading.
- **New subs2srs deck imported in Anki?** Nothing to do while `mdb serve` runs: the sync picks it
  up and parses it. Otherwise run `./mdb sync`.
- **New or changed word list in a `*_daily` add-on folder?** `./mdb import-tagsets`
- **Retire a tag set:** `./mdb drop-tagset '!prefix'`. Re-imports skip it.
- **Mark a word:** `./mdb known 分かる`, `./mdb unknown 方 かた`, `./mdb forget …`. Or use the Words tab.
- **Bulk marks:** `./mdb mark-file FILE` — one `ACTION<TAB>BASE<TAB>READING` per line (known / unknown / forget / ignore / unignore / auto), one recount for the lot.
- **Restore a backup:** `scp root@89.167.79.200:/var/backups/fazon-db/<file>.db.zst . && zstd -d <file>.db.zst`,
  then put it at `data/fazon.db` with `mdb serve` stopped.

## Service

```
cp fazon-db.service ~/.config/systemd/user/ && systemctl --user enable --now fazon-db
```
