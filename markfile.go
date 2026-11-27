package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// cmdMarkFile applies many word marks at once: every mark is written in one
// transaction, then known status, i+N and tag-set staleness are refreshed once
// (markWord does that per word). Exact (base, reading) only; unknown words are
// reported, not guessed.
func cmdMarkFile(db *sql.DB, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	type mark struct{ how, base, reading string }
	var marks []mark
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.Split(line, "\t")
		if len(p) != 3 {
			return fmt.Errorf("%s:%d: want ACTION<TAB>BASE<TAB>READING", path, ln)
		}
		switch p[0] {
		case "known", "unknown", "forget", "ignore", "unignore", "auto":
		default:
			return fmt.Errorf("%s:%d: unknown action %q", path, ln, p[0])
		}
		marks = append(marks, mark{p[0], p[1], p[2]})
	}
	if err := sc.Err(); err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	t0 := time.Now()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ids []int64
	missing := 0
	now := time.Now().Unix()
	for _, m := range marks {
		var id int64
		if err := tx.QueryRow(`SELECT id FROM morphs WHERE base=? AND reading=?`, m.base, m.reading).Scan(&id); err != nil {
			fmt.Fprintf(os.Stderr, "no word %s %s\n", m.base, m.reading)
			missing++
			continue
		}
		ids = append(ids, id)
		var q string
		var args []any
		switch m.how {
		case "ignore", "unignore":
			q = `INSERT INTO ignore_marks(base,reading,ignored,at) VALUES(?,?,?,?) ON CONFLICT(base,reading) DO UPDATE SET ignored=excluded.ignored, at=excluded.at`
			args = []any{m.base, m.reading, b2i(m.how == "ignore"), now}
		case "auto":
			q, args = `DELETE FROM ignore_marks WHERE base=? AND reading=?`, []any{m.base, m.reading}
		case "forget":
			q, args = `DELETE FROM manual_known WHERE base=? AND reading=?`, []any{m.base, m.reading}
		default:
			q = `INSERT INTO manual_known(base,reading,known,at) VALUES(?,?,?,?) ON CONFLICT(base,reading) DO UPDATE SET known=excluded.known, at=excluded.at`
			args = []any{m.base, m.reading, b2i(m.how == "known"), now}
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	flipped, err := refreshKnown(db, ids)
	if err != nil {
		return err
	}
	n, err := refreshNotes(db, flipped)
	if err != nil {
		return err
	}
	fmt.Printf("%d marks (%d not found): %d words flipped, %d notes recounted in %s\n",
		len(ids), missing, len(flipped), n, time.Since(t0).Round(time.Millisecond))
	return nil
}
