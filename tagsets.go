package main

// Mining tag sets: the Go port of the *_daily Anki add-ons (engine.py).
//
//   <prefix>::NNNN::PP   NNNN = the target word's rank in the set's list,
//                        PP   = alternate position (01 = best card)
//
// A candidate is a pure i+1 pool note whose single unknown word is the target
// and whose sentence has real context. Candidates are ordered round-robin
// across creators (each creator's cards closest-to-ideal-length first),
// capped, with one zatsudan card guaranteed when the set has a zatsudan glob.
// Unlike the add-ons this runs on every change, not once a day: learn a word
// and its tags are gone on the next review sync.

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type tagset struct {
	id                          int64
	prefix, name                string
	pool                        []string
	creators                    [][2]string
	zatsudan                    string
	cap, ideal, minLen, maxLen  int
	enabled                     bool
	targets                     []target
}

type target struct {
	rank int
	word string
}

// ---------------------------------------------------------------------------
// deck globs (Anki "deck:" semantics: * wildcard, matches subdecks too)

var globCache = map[string]*regexp.Regexp{}

func deckGlob(g string) *regexp.Regexp {
	if re, ok := globCache[g]; ok {
		return re
	}
	g = strings.Trim(strings.TrimPrefix(strings.Trim(g, `"`), "deck:"), `"`)
	parts := strings.Split(g, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile(`(?i)^` + strings.Join(parts, `.*`) + `(::.*)?$`)
	globCache[g] = re
	return re
}

func matchAny(globs []string, name string) bool {
	for _, g := range globs {
		if deckGlob(g).MatchString(name) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// import from the add-on folders

func readCSVColumn(path, col string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		return nil
	}
	idx := -1
	for i, h := range recs[0] {
		if strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")) == col {
			idx = i
		}
	}
	if idx < 0 {
		return nil
	}
	var out []string
	for _, rec := range recs[1:] {
		if idx < len(rec) && strings.TrimSpace(rec[idx]) != "" {
			out = append(out, strings.TrimSpace(rec[idx]))
		}
	}
	return out
}

func readTargets(path string) ([]target, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	ri, wi := -1, -1
	for i, h := range recs[0] {
		switch strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")) {
		case "rank":
			ri = i
		case "word":
			wi = i
		}
	}
	if wi < 0 {
		return nil, fmt.Errorf("%s: no word column", path)
	}
	var out []target
	for _, rec := range recs[1:] {
		if wi >= len(rec) || strings.TrimSpace(rec[wi]) == "" {
			continue
		}
		rank := len(out) + 1
		if ri >= 0 && ri < len(rec) {
			if n, err := strconv.Atoi(strings.TrimSpace(rec[ri])); err == nil {
				rank = n
			}
		}
		out = append(out, target{rank, strings.TrimSpace(rec[wi])})
	}
	return out, nil
}

// importTagsets reads every add-on folder that has an engine-style config
// (tag_prefix + pool_queries + targets_csv) and upserts it as a tag set.
// Re-running refreshes target lists from the add-on folders.
func importTagsets(db *sql.DB) error {
	cfgs, _ := filepath.Glob(filepath.Join(addonsDir, "*", "config.json"))
	n := 0
	for _, cp := range cfgs {
		b, err := os.ReadFile(cp)
		if err != nil {
			continue
		}
		var c struct {
			TagPrefix    string     `json:"tag_prefix"`
			Friendly     string     `json:"friendly_name"`
			Pool         []string   `json:"pool_queries"`
			Creators     [][]string `json:"creators"`
			Zatsudan     string     `json:"zatsudan_query"`
			TargetsCSV   string     `json:"targets_csv"`
			Cap          int        `json:"cap"`
			Ideal        int        `json:"ideal_len"`
			Min          int        `json:"min_len"`
			Max          int        `json:"max_len"`
			RunDaily     *bool      `json:"run_automatically_daily"`
		}
		if json.Unmarshal(b, &c) != nil || c.TagPrefix == "" || c.TargetsCSV == "" || len(c.Pool) == 0 {
			continue
		}
		dir := filepath.Dir(cp)
		tp := c.TargetsCSV
		if !filepath.IsAbs(tp) {
			tp = filepath.Join(dir, tp)
		}
		targets, err := readTargets(tp)
		if err != nil {
			log.Printf("tagset %s: %v", c.TagPrefix, err)
			continue
		}
		creators := make([][2]string, 0, len(c.Creators))
		for _, cr := range c.Creators {
			if len(cr) == 2 {
				creators = append(creators, [2]string{cr[0], cr[1]})
			}
		}
		pj, _ := json.Marshal(c.Pool)
		cj, _ := json.Marshal(creators)
		def := func(v, d int) int {
			if v == 0 {
				return d
			}
			return v
		}
		tx, _ := db.Begin()
		_, err = tx.Exec(`INSERT INTO tagsets(prefix,name,pool,creators,zatsudan,cap,ideal_len,min_len,max_len,source)
		   VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(prefix) DO UPDATE SET name=excluded.name, pool=excluded.pool,
		   creators=excluded.creators, zatsudan=excluded.zatsudan, cap=excluded.cap, ideal_len=excluded.ideal_len,
		   min_len=excluded.min_len, max_len=excluded.max_len, source=excluded.source`,
			c.TagPrefix, c.Friendly, string(pj), string(cj), c.Zatsudan, def(c.Cap, 8), def(c.Ideal, 18), def(c.Min, 8), def(c.Max, 45), filepath.Base(dir))
		if err != nil {
			tx.Rollback()
			return err
		}
		var id int64
		tx.QueryRow(`SELECT id FROM tagsets WHERE prefix=?`, c.TagPrefix).Scan(&id)
		tx.Exec(`DELETE FROM tagset_targets WHERE tagset_id=?`, id)
		for _, t := range targets {
			tx.Exec(`INSERT OR IGNORE INTO tagset_targets(tagset_id, rank, word) VALUES(?,?,?)`, id, t.rank, t.word)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		n++
	}
	log.Printf("tag sets imported from add-ons: %d", n)
	return nil
}

func tagsetPrefixes(db *sql.DB) []string {
	rows, err := db.Query(`SELECT prefix FROM tagsets`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

func loadTagsets(db *sql.DB) ([]*tagset, error) {
	rows, err := db.Query(`SELECT id, prefix, COALESCE(name,''), pool, creators, zatsudan, cap, ideal_len, min_len, max_len, enabled FROM tagsets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []*tagset
	for rows.Next() {
		t := &tagset{}
		var pool, creators string
		rows.Scan(&t.id, &t.prefix, &t.name, &pool, &creators, &t.zatsudan, &t.cap, &t.ideal, &t.minLen, &t.maxLen, &t.enabled)
		json.Unmarshal([]byte(pool), &t.pool)
		json.Unmarshal([]byte(creators), &t.creators)
		out = append(out, t)
	}
	rows.Close()
	for _, t := range out {
		r, err := db.Query(`SELECT rank, word FROM tagset_targets WHERE tagset_id=? ORDER BY rank`, t.id)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var x target
			r.Scan(&x.rank, &x.word)
			t.targets = append(t.targets, x)
		}
		r.Close()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// compute

var speakerLabel = regexp.MustCompile(`^[^：:\n]{1,8}[：:]`)

func labelArtifact(sent, word string) bool {
	return strings.Contains(sent, word) && !strings.Contains(speakerLabel.ReplaceAllString(sent, ""), word)
}

func runeLen(s string) int { return contentLen(s) }

type cand struct {
	id    int64
	deck  string
	sent  string
	clen  int
	base  string
}

// minedWords = words already mined into a non-pool deck but not yet studied
// (still unknown), so their pool alternates should not keep showing up.
func minedWords(db *sql.DB) map[string]bool {
	out := map[string]bool{}
	rows, err := db.Query(`SELECT n.word FROM notes n JOIN decks d ON d.id = n.deck_id
	   WHERE d.is_pool = 0 AND n.word != '' AND NOT EXISTS (SELECT 1 FROM cards c WHERE c.note_id = n.id AND c.type != 0)`)
	if err == nil {
		for rows.Next() {
			var w string
			rows.Scan(&w)
			if b := parseText("Word", "", w); b != "" {
				out[b] = true
			}
		}
		rows.Close()
	}
	rows, err = db.Query(`SELECT word FROM mined WHERE word IS NOT NULL`)
	if err == nil {
		for rows.Next() {
			var w string
			rows.Scan(&w)
			out[w] = true
		}
		rows.Close()
	}
	return out
}

func computeTagsets(db *sql.DB) error {
	t0 := time.Now()
	sets, err := loadTagsets(db)
	if err != nil {
		return err
	}
	decks := map[int64]string{}
	rows, err := db.Query(`SELECT id, name FROM decks`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var n string
		rows.Scan(&id, &n)
		decks[id] = n
	}
	rows.Close()

	// every pure i+1 note, grouped by its unknown word's dictionary form
	byBase := map[string][]cand{}
	rows, err = db.Query(`SELECT n.id, COALESCE(n.deck_id,0), COALESCE(n.sentence,''), n.clen, m.base
	   FROM notes n JOIN morphs m ON m.id = n.unk_morph WHERE n.n_unknown = 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c cand
		var did int64
		rows.Scan(&c.id, &did, &c.sent, &c.clen, &c.base)
		c.deck = decks[did]
		byBase[c.base] = append(byBase[c.base], c)
	}
	rows.Close()
	mined := minedWords(db)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, _ := tx.Prepare(`INSERT INTO tagset_assign(tagset_id, rank, pos, note_id, word, tag) VALUES(?,?,?,?,?,?)`)
	var summary []string
	for _, ts := range sets {
		tx.Exec(`DELETE FROM tagset_assign WHERE tagset_id=?`, ts.id)
		if !ts.enabled {
			continue
		}
		placed, tags := 0, 0
		for _, tg := range ts.targets {
			if mined[tg.word] {
				continue
			}
			wl := runeLen(tg.word)
			var cs []cand
			for _, c := range byBase[tg.word] {
				if !matchAny(ts.pool, c.deck) {
					continue
				}
				if labelArtifact(c.sent, tg.word) {
					continue
				}
				lo := ts.minLen
				if wl+4 > lo {
					lo = wl + 4
				}
				if c.clen < lo || c.clen > ts.maxLen {
					continue
				}
				cs = append(cs, c)
			}
			ordered := orderCands(ts, cs)
			if len(ordered) == 0 {
				continue
			}
			placed++
			for i, c := range ordered {
				tag := fmt.Sprintf("%s::%04d::%02d", ts.prefix, tg.rank, i+1)
				if _, err := ins.Exec(ts.id, tg.rank, i+1, c.id, tg.word, tag); err != nil {
					return err
				}
				tags++
			}
		}
		summary = append(summary, fmt.Sprintf("%s %d/%d words %d tags", ts.prefix, placed, len(ts.targets), tags))
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	setMeta(db, "tagsets_built", time.Now().Format(time.RFC3339))
	log.Printf("tag sets recomputed in %s: %s", time.Since(t0).Round(time.Millisecond), strings.Join(summary, "; "))
	return nil
}

func orderCands(ts *tagset, cs []cand) []cand {
	if len(cs) == 0 {
		return nil
	}
	creatorOf := func(deck string) string {
		for _, cr := range ts.creators {
			if deckGlob(cr[1]).MatchString(deck) {
				return cr[0]
			}
		}
		return "?"
	}
	by := map[string][]cand{}
	for _, c := range cs {
		k := creatorOf(c.deck)
		by[k] = append(by[k], c)
	}
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	for _, l := range by {
		sort.Slice(l, func(i, j int) bool {
			di, dj := abs(l[i].clen-ts.ideal), abs(l[j].clen-ts.ideal)
			if di != dj {
				return di < dj
			}
			return l[i].id < l[j].id
		})
	}
	var order []string
	for _, cr := range ts.creators {
		if _, ok := by[cr[0]]; ok && !contains(order, cr[0]) {
			order = append(order, cr[0])
		}
	}
	var rest []string
	for k := range by {
		if !contains(order, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	idx := map[string]int{}
	var full []cand
	for len(full) < len(cs) {
		progressed := false
		for _, cr := range order {
			if idx[cr] < len(by[cr]) {
				full = append(full, by[cr][idx[cr]])
				idx[cr]++
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	out := full
	if len(out) > ts.cap {
		out = append([]cand(nil), full[:ts.cap]...)
	}
	if ts.zatsudan != "" {
		z := deckGlob(ts.zatsudan)
		has := false
		for _, c := range out {
			if z.MatchString(c.deck) {
				has = true
			}
		}
		if !has {
			for _, c := range full {
				if z.MatchString(c.deck) {
					if len(out) < ts.cap {
						out = append(out, c)
					} else {
						out[len(out)-1] = c
					}
					break
				}
			}
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
