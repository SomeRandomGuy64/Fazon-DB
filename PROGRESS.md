# mining-db — build progress (resume here if a session is cut off)

Request (2026-10-19): local SQLite mining DB (Go) that replaces AnkiMorphs and, long-term,
Anki itself for everything except reviews. Parser = Faizan's reading model
(jev_parser_test/models/best via reading_disambig). Japanese only.

## Done (2026-10-19)
- Import from Anki (40 s, 1,020,814 notes + cards + tags + MvJ context + AnkiMorphs known lemmas)
- Full parse with his model: 11 min (3 GPU workers, ~1,500 cards/s); `mdb parse` resumes
- Lemma matching (UniDic lemma) for word lists + AnkiMorphs known → parity with Anki:
  i+0/i+1/i+≥2 747k/219k/54k (Anki 727k/225k/64k); 6,047 known (base,reading) pairs
- `learned` table: every known word is saved for good (2,988 review + 3,059 AnkiMorphs)
- 11 tag sets recomputed in ~9 s on every change; video157 + vampire-lyrics DROPPED (user)
- Own media store: 2,046,865 files hard-linked into media/ (no extra disk); 223 Vivi
  screenshots exist nowhere (apkgs deleted long ago)
- Direct .apkg import (tested on a scratch copy: 1,002 cards + 2,004 media in 31 s)
- /mvj-mine: precheck-db → `mdb prepare` (fills Word in Anki, or creates the card straight in
  Study Deck when the note only lives in the DB) → bridge `mine 'NID:TAG'` → `mdb mined`
- Harness http://127.0.0.1:8790 (systemd user service `mining-db`, live sync every minute)
- Daily backup to root@89.167.79.200:/var/backups/mining-db (151 MB, restore verified)
- Portable export (notes.jsonl + known_words.csv) + import-known for any future SRS

## Waiting on the user
- `mdb anki-prune -apply`: remove the ~1.02M Database:: notes from Anki (dry run first)
- After that: retire AnkiMorphs + the *_daily add-ons in Anki (the DB does their job now)
