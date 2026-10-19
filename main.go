// mdb — mining-db: Faizan's local sentence-mining database.
//
// SQLite store of every subs2srs (Database::*) and mined MvJ card, tokenized
// with his reading-disambiguation model, with known words, i+N, a frequency
// priority list and the mining tag sets maintained incrementally.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const usage = `mdb — mining-db

Setup / rebuild
  mdb import                 pull notes, cards, tags, context, AnkiMorphs known lemmas, tag sets from Anki
  mdb parse [-j 3]           tokenize notes whose text or parser model changed (resumable)
  mdb reparse [-j 3]         re-tokenize everything (after improving the model)
  mdb recalc                 full rebuild of priority list, known words, i+N, tag sets
  mdb import-tagsets         re-read the *_daily add-on configs + targets.csv
  mdb drop-tagset PREFIX     remove a tag set for good (re-imports skip it)

Day to day
  mdb serve [-addr :8790]    web harness + live Anki sync every minute
  mdb sync [-full] [-days 1] one sync pass with Anki (incremental recalc)
  mdb status                 counts, model freshness, last sync/backup
  mdb search QUERY [-n 20]   search notes (see query.go for syntax)
  mdb known BASE [READING]   mark a word known       (unknown / forget likewise)
  mdb backup                 snapshot + upload to Hetzner

Long-term (database first, Anki only for reviews)
  mdb import-apkg FILE.apkg [-deck NAME]   add a subs2srs deck straight to the database
  mdb media-adopt            hard-link every pool clip/screenshot into mining-db/media
  mdb export DIR [QUERY]     portable dump (notes.jsonl + known_words.csv) for any SRS
  mdb import-known FILE      mark words known from any app's export (word[,reading] per line)
  mdb anki-prune [-deck G] [-apply]   remove pool notes from Anki that the database fully holds

Mining (/mvj-mine)
  mdb alternates TAG         JSON: this card + later alternates of the word
  mdb prepare NID [WORD]     ensure note is in Anki with Word filled; prints the Anki nid
  mdb mined NID TAG [WORD]   record a mined card
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(1)
	}
	cmd, args := os.Args[1], os.Args[2:]
	db, err := openDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := run(db, cmd, args); err != nil {
		log.Fatalf("%s: %v", cmd, err)
	}
}

func run(db *sql.DB, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	jobs := fs.Int("j", 3, "parser workers")
	addr := fs.String("addr", "127.0.0.1:8790", "listen address")
	full := fs.Bool("full", false, "full sync")
	days := fs.Int("days", 1, "sync window in days")
	limit := fs.Int("n", 20, "result limit")
	deck := fs.String("deck", "", "deck name / glob")
	apply := fs.Bool("apply", false, "actually do it")
	fs.Parse(args)
	rest := fs.Args()

	switch cmd {
	case "import":
		return cmdImport(db)
	case "parse":
		if err := cmdParse(db, *jobs, false); err != nil {
			return err
		}
		return recalcAll(db)
	case "reparse":
		if err := cmdParse(db, *jobs, true); err != nil {
			return err
		}
		return recalcAll(db)
	case "recalc":
		return recalcAll(db)
	case "import-tagsets":
		if err := importTagsets(db); err != nil {
			return err
		}
		return computeTagsets(db)
	case "tagsets":
		return computeTagsets(db)
	case "drop-tagset":
		for _, p := range rest {
			if err := dropTagset(db, p); err != nil {
				return err
			}
			fmt.Println("dropped", p)
		}
		return nil
	case "sync":
		st, err := syncOnce(db, *days, *full)
		if err != nil {
			return err
		}
		fmt.Println(st)
		return nil
	case "serve":
		return serve(db, *addr)
	case "status":
		return printStatus(db)
	case "search":
		rs, err := search(db, strings.Join(rest, " "), *limit, 0)
		if err != nil {
			return err
		}
		printJSON(rs)
		return nil
	case "known", "unknown", "forget":
		if len(rest) < 1 {
			return fmt.Errorf("usage: mdb %s BASE [READING]", cmd)
		}
		reading := ""
		if len(rest) > 1 {
			reading = rest[1]
		}
		n, err := markWord(db, rest[0], reading, cmd)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %d word(s) updated\n", cmd, n)
		return nil
	case "backup":
		return cmdBackup(db)
	case "import-apkg":
		if len(rest) < 1 {
			return fmt.Errorf("usage: mdb import-apkg FILE.apkg [-deck NAME]")
		}
		for _, f := range rest {
			if err := cmdImportApkg(db, f, *deck); err != nil {
				return err
			}
		}
		return nil
	case "media-adopt":
		return cmdMediaAdopt(db)
	case "export":
		if len(rest) < 1 {
			return fmt.Errorf("usage: mdb export DIR [QUERY]")
		}
		return cmdExport(db, rest[0], strings.Join(rest[1:], " "))
	case "import-known":
		if len(rest) != 1 {
			return fmt.Errorf("usage: mdb import-known FILE")
		}
		return cmdImportKnown(db, rest[0])
	case "anki-prune":
		return cmdAnkiPrune(db, *deck, *apply)
	case "alternates":
		if len(rest) != 1 {
			return fmt.Errorf("usage: mdb alternates TAG")
		}
		out, err := cmdAlternates(db, rest[0])
		if err != nil {
			return err
		}
		printJSON(out)
		return nil
	case "prepare":
		if len(rest) < 1 {
			return fmt.Errorf("usage: mdb prepare NID [WORD]")
		}
		nid, _ := strconv.ParseInt(rest[0], 10, 64)
		word := ""
		if len(rest) > 1 {
			word = rest[1]
		}
		id, err := prepareMine(db, nid, word)
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	case "mined":
		if len(rest) < 2 {
			return fmt.Errorf("usage: mdb mined NID TAG [WORD]")
		}
		nid, _ := strconv.ParseInt(rest[0], 10, 64)
		word := wordOfTag(db, rest[1])
		if len(rest) > 2 {
			word = rest[2]
		}
		return recordMined(db, nid, rest[1], word)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command")
	}
}

// markWord sets a manual known/unknown override (or clears it) for every
// reading of BASE, or just one reading, then updates i+N and tag sets.
func markWord(db *sql.DB, base, reading, how string) (int, error) {
	writeMu.Lock()
	defer writeMu.Unlock()
	q := `SELECT id, base, reading FROM morphs WHERE base=?`
	args := []any{base}
	if reading != "" {
		q += ` AND reading=?`
		args = append(args, reading)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	type br struct{ b, r string }
	var keys []br
	for rows.Next() {
		var id int64
		var b, r string
		rows.Scan(&id, &b, &r)
		ids = append(ids, id)
		keys = append(keys, br{b, r})
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, fmt.Errorf("no word %q %q in the database", base, reading)
	}
	for _, k := range keys {
		switch how {
		case "forget":
			db.Exec(`DELETE FROM manual_known WHERE base=? AND reading=?`, k.b, k.r)
		default:
			v := 0
			if how == "known" {
				v = 1
			}
			db.Exec(`INSERT INTO manual_known(base,reading,known,at) VALUES(?,?,?,?) ON CONFLICT(base,reading) DO UPDATE SET known=excluded.known, at=excluded.at`,
				k.b, k.r, v, time.Now().Unix())
		}
	}
	flipped, err := refreshKnown(db, ids)
	if err != nil {
		return 0, err
	}
	if len(flipped) > 0 {
		if _, err := refreshNotes(db, flipped); err != nil {
			return 0, err
		}
		if err := computeTagsets(db); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
