package main

import (
	"bufio"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// modelFingerprint identifies the parser model, so a model upgrade shows up as
// "N notes parsed with an older model" and `mdb reparse` redoes them.
func modelFingerprint() string {
	h := sha1.New()
	for _, f := range []string{"rd_config.json", "extras.json", "model.safetensors"} {
		p := filepath.Join(modelDir, f)
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "%s:%d:%d;", f, st.Size(), st.ModTime().Unix())
		}
	}
	if b, err := os.ReadFile(filepath.Join(modelDir, "rd_config.json")); err == nil {
		h.Write(b)
	}
	if b, err := os.ReadFile(workerPy); err == nil { // filtering rules live in the worker
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// ---------------------------------------------------------------------------
// worker process

type parseLine struct {
	ID   int64  `json:"id"`
	Text string `json:"text"`
}

// tok = [surface, base, reading, pos1, pos2, pos3, keep]
type parseResult struct {
	ID   int64   `json:"id"`
	Toks [][]any `json:"toks"`
}

type worker struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
	mu  sync.Mutex
}

func startWorker() (*worker, error) {
	cmd := exec.Command("python3", workerPy)
	cmd.Env = append(os.Environ(), "PYTHONWARNINGS=ignore", "TOKENIZERS_PARALLELISM=false")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outp, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	logf, _ := os.OpenFile(filepath.Join(filepath.Dir(dbPath), "parser.stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &worker{cmd: cmd, in: in, out: bufio.NewReaderSize(outp, 1<<20)}
	line, err := w.out.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("parser worker failed to start (see data/parser.stderr.log): %w", err)
	}
	if !strings.Contains(line, `"ready"`) {
		return nil, fmt.Errorf("parser worker: unexpected first line %q", line)
	}
	return w, nil
}

func (w *worker) parse(lines []parseLine) ([]parseResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"lines": lines})
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	resp, err := w.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var r struct {
		Results []parseResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(resp), &r); err != nil {
		return nil, err
	}
	return r.Results, nil
}

func (w *worker) close() {
	w.in.Close()
	w.cmd.Wait()
}

// ---------------------------------------------------------------------------
// what gets parsed

var (
	reHTML     = regexp.MustCompile(`<[^>]+>`)
	rePitch    = regexp.MustCompile(`:[\x21-\x7e]*`) // MvJ Word field ":A:hl-" codes
	reFurigana = regexp.MustCompile(`\[[^\]]*\]`)
)

func parseText(field, sentence, word string) string {
	if field == "Word" {
		w := reFurigana.ReplaceAllString(word, "")
		w = rePitch.ReplaceAllString(w, "")
		w = strings.ReplaceAll(strings.ReplaceAll(w, "/", " "), "*", "")
		return strings.Join(strings.Fields(reHTML.ReplaceAllString(w, "")), "")
	}
	return sentence
}

const punct = "。、！？!?…‥・ー～〜「」『』（）()【】,. 　wｗWＷ"

func contentLen(s string) int {
	s = reHTML.ReplaceAllString(s, "")
	n := 0
	for _, r := range s {
		if !strings.ContainsRune(punct, r) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// morph id cache

type morphCache struct {
	ids      map[string]int64
	lemmaSet map[int64]bool
	db       *sql.DB
}

func loadMorphCache(db *sql.DB) *morphCache {
	c := &morphCache{ids: map[string]int64{}, lemmaSet: map[int64]bool{}, db: db}
	rows, _ := db.Query(`SELECT id, base, reading, lemma IS NOT NULL FROM morphs`)
	for rows.Next() {
		var id int64
		var b, r string
		var hl bool
		rows.Scan(&id, &b, &r, &hl)
		c.ids[b+"\x00"+r] = id
		c.lemmaSet[id] = hl
	}
	rows.Close()
	return c
}

func (c *morphCache) id(tx *sql.Tx, base, reading, pos, lemma string) (int64, error) {
	k := base + "\x00" + reading
	if id, ok := c.ids[k]; ok {
		if !c.lemmaSet[id] && lemma != "" {
			tx.Exec(`UPDATE morphs SET lemma=? WHERE id=? AND lemma IS NULL`, lemma, id)
			c.lemmaSet[id] = true
		}
		return id, nil
	}
	res, err := tx.Exec(`INSERT INTO morphs(base, reading, pos, lemma) VALUES(?,?,?,NULLIF(?, ''))`, base, reading, pos, lemma)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	c.ids[k] = id
	c.lemmaSet[id] = lemma != ""
	return id, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// storeResults writes tokens + note_morphs for one batch. Returns the set of
// morph ids touched (old and new), for incremental status refresh.
func storeResults(tx *sql.Tx, mc *morphCache, fp string, texts map[int64]string, sentences map[int64]string, res []parseResult) (map[int64]bool, error) {
	touched := map[int64]bool{}
	for _, r := range res {
		rows, err := tx.Query(`SELECT morph_id FROM note_morphs WHERE note_id=?`, r.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m int64
			rows.Scan(&m)
			touched[m] = true
		}
		rows.Close()
		if _, err := tx.Exec(`DELETE FROM note_morphs WHERE note_id=?`, r.ID); err != nil {
			return nil, err
		}
		counts := map[int64]int{}
		var order []int64
		toks := make([][2]any, 0, len(r.Toks))
		for _, t := range r.Toks {
			if len(t) < 7 {
				continue
			}
			surface, base, reading, pos := str(t[0]), str(t[1]), str(t[2]), str(t[3])
			keep, _ := t[6].(float64)
			var mid int64
			if keep == 1 && base != "" {
				if reading == "" || reading == "?" {
					reading = base
				}
				lemma := ""
				if len(t) > 7 {
					lemma = str(t[7])
				}
				mid, err = mc.id(tx, base, reading, pos, lemma)
				if err != nil {
					return nil, err
				}
				if counts[mid] == 0 {
					order = append(order, mid)
				}
				counts[mid]++
				touched[mid] = true
			}
			toks = append(toks, [2]any{surface, mid})
		}
		for _, m := range order {
			if _, err := tx.Exec(`INSERT INTO note_morphs(note_id, morph_id, cnt) VALUES(?,?,?)`, r.ID, m, counts[m]); err != nil {
				return nil, err
			}
		}
		tj, _ := json.Marshal(toks)
		if _, err := tx.Exec(`UPDATE notes SET parsed_model=?, parsed_text=?, tokens=?, n_morphs=?, clen=? WHERE id=?`,
			fp, texts[r.ID], string(tj), len(order), contentLen(sentences[r.ID]), r.ID); err != nil {
			return nil, err
		}
	}
	return touched, nil
}

// ---------------------------------------------------------------------------
// bulk parse

type pendingNote struct {
	id       int64
	deck     int64
	text     string
	sentence string
}

// cmdParse parses every note whose text or parser model changed.
// all=true reparses everything.
func cmdParse(db *sql.DB, nWorkers int, all bool) error {
	fp := modelFingerprint()
	q := `SELECT id, COALESCE(deck_id,0), parse_field, COALESCE(sentence,''), COALESCE(word,''), COALESCE(parsed_model,''), COALESCE(parsed_text,'') FROM notes ORDER BY deck_id, id`
	rows, err := db.Query(q)
	if err != nil {
		return err
	}
	var todo []pendingNote
	for rows.Next() {
		var id, deck int64
		var field, sent, word, pm, pt string
		rows.Scan(&id, &deck, &field, &sent, &word, &pm, &pt)
		text := parseText(field, sent, word)
		if all || pm != fp || pt != text {
			todo = append(todo, pendingNote{id, deck, text, sent})
		}
	}
	rows.Close()
	if len(todo) == 0 {
		log.Printf("parse: nothing to do (model %s)", fp)
		return nil
	}
	log.Printf("parse: %d notes with model %s, %d workers", len(todo), fp, nWorkers)

	// batches never straddle decks, so neighbour context stays within one video
	const batchMax = 384
	var batches [][]pendingNote
	for i := 0; i < len(todo); {
		j := i
		for j < len(todo) && j-i < batchMax && todo[j].deck == todo[i].deck {
			j++
		}
		batches = append(batches, todo[i:j])
		i = j
	}

	type done struct {
		batch []pendingNote
		res   []parseResult
	}
	jobs := make(chan []pendingNote)
	results := make(chan done, nWorkers*2)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for w := 0; w < nWorkers; w++ {
		wk, err := startWorker()
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(wk *worker) {
			defer wg.Done()
			defer wk.close()
			for b := range jobs {
				lines := make([]parseLine, len(b))
				for i, n := range b {
					lines[i] = parseLine{n.id, n.text}
				}
				res, err := wk.parse(lines)
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				results <- done{b, res}
			}
		}(wk)
	}
	go func() {
		for _, b := range batches {
			jobs <- b
		}
		close(jobs)
	}()
	go func() { wg.Wait(); close(results) }()

	mc := loadMorphCache(db)
	t0 := time.Now()
	n := 0
	tx, _ := db.Begin()
	pendingCommit := 0
	for d := range results {
		texts, sents := map[int64]string{}, map[int64]string{}
		for _, p := range d.batch {
			texts[p.id] = p.text
			sents[p.id] = p.sentence
		}
		if _, err := storeResults(tx, mc, fp, texts, sents, d.res); err != nil {
			tx.Rollback()
			return err
		}
		n += len(d.batch)
		pendingCommit += len(d.batch)
		if pendingCommit >= 20000 {
			if err := tx.Commit(); err != nil {
				return err
			}
			tx, _ = db.Begin()
			pendingCommit = 0
			el := time.Since(t0)
			rate := float64(n) / el.Seconds()
			log.Printf("parse: %d/%d (%.0f/s, ~%s left)", n, len(todo), rate,
				(time.Duration(float64(len(todo)-n)/rate) * time.Second).Round(time.Minute))
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if firstErr != nil {
		return fmt.Errorf("parse stopped after %d notes: %w (rerun to resume)", n, firstErr)
	}
	log.Printf("parse: done %d notes in %s", n, time.Since(t0).Round(time.Second))
	setMeta(db, "parsed_model", fp)
	return nil
}
