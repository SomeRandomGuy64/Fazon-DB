package main

// Web harness: a local UI over the database + the live Anki sync loop.

import (
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

func serve(db *sql.DB, addr string) error {
	go syncLoop(db, time.Minute)
	go backupLoop(db)

	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, getStatus(db))
	})
	mux.HandleFunc("/api/search", func(w http.ResponseWriter, r *http.Request) {
		lim, off := intParam(r, "limit", 50), intParam(r, "offset", 0)
		rs, err := search(db, r.URL.Query().Get("q"), lim, off)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, rs)
	})
	mux.HandleFunc("/api/note/", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/note/"), 10, 64)
		n, err := noteDetail(db, id)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, n)
	})
	mux.HandleFunc("/api/words", func(w http.ResponseWriter, r *http.Request) {
		ws, err := listWords(db, r.URL.Query().Get("q"), r.URL.Query().Get("known"), intParam(r, "limit", 100), intParam(r, "offset", 0))
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, ws)
	})
	mux.HandleFunc("/api/word/mark", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", 405)
			return
		}
		var req struct{ Base, Reading, How string }
		json.NewDecoder(r.Body).Decode(&req)
		if req.How != "known" && req.How != "unknown" && req.How != "forget" {
			http.Error(w, "how must be known|unknown|forget", 400)
			return
		}
		n, err := markWord(db, req.Base, req.Reading, req.How)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]int{"updated": n})
	})
	mux.HandleFunc("/api/tagset", func(w http.ResponseWriter, r *http.Request) {
		rs, err := tagsetView(db, r.URL.Query().Get("prefix"), intParam(r, "limit", 50), intParam(r, "offset", 0))
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, rs)
	})
	mux.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", 405)
			return
		}
		st, err := syncOnce(db, 1, r.URL.Query().Get("full") == "1")
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"result": st.String()})
	})
	mux.HandleFunc("/media/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(strings.TrimPrefix(r.URL.Path, "/media/"))
		p := mediaPath(name)
		if p == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "max-age=86400")
		http.ServeFile(w, r, p)
	})
	log.Printf("mining-db harness on http://%s", addr)
	return http.ListenAndServe(addr, mux)
}

// backupLoop runs `backup` once a day (the first check is 10 min after start).
func backupLoop(db *sql.DB) {
	if os.Getenv("MDB_NO_BACKUP") != "" {
		return
	}
	time.Sleep(10 * time.Minute)
	for {
		last, _ := time.Parse(time.RFC3339, getMeta(db, "last_backup"))
		if time.Since(last) > 23*time.Hour {
			if err := cmdBackup(db); err != nil {
				log.Printf("backup: %v", err)
			}
		}
		time.Sleep(time.Hour)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), 500)
}

func intParam(r *http.Request, k string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil && v >= 0 {
		if v > 1000 {
			v = 1000
		}
		return v
	}
	return def
}

// ---------------------------------------------------------------------------

type tokenOut struct {
	Surface string `json:"s"`
	MorphID int64  `json:"id,omitempty"`
	Base    string `json:"base,omitempty"`
	Reading string `json:"reading,omitempty"`
	Known   bool   `json:"known"`
	Src     string `json:"src,omitempty"`
	Rank    int    `json:"rank,omitempty"`
}

type noteFull struct {
	ID              int64      `json:"nid"`
	Deck            string     `json:"deck"`
	InAnki          bool       `json:"in_anki"`
	Fields          map[string]string `json:"fields"`
	Summary         string     `json:"summary"`
	ContextLines    string     `json:"context_lines"`
	Tokens          []tokenOut `json:"tokens"`
	NUnknown        int        `json:"n_unknown"`
	Tags            []string   `json:"tags"`
	Cards           []map[string]any `json:"cards"`
	ParsedModel     string     `json:"parsed_model"`
}

func noteDetail(db *sql.DB, id int64) (noteFull, error) {
	n := noteFull{Fields: map[string]string{}}
	var inAnki int
	var toks string
	var f [11]string
	err := db.QueryRow(`SELECT n.id, COALESCE(d.name,''), n.in_anki, COALESCE(n.sentence,''), COALESCE(n.sentence_audio,''),
	   COALESCE(n.word,''), COALESCE(n.word_audio,''), COALESCE(n.definition,''), COALESCE(n.definition_audio,''),
	   COALESCE(n.am_study_morphs,''), COALESCE(n.image,''), COALESCE(n.notes,''), COALESCE(n.context,''),
	   COALESCE(s.text,''), COALESCE(n.ctx_lines,''), COALESCE(n.tokens,'[]'), n.n_unknown, COALESCE(n.parsed_model,'')
	   FROM notes n LEFT JOIN decks d ON d.id=n.deck_id LEFT JOIN summaries s ON s.id=n.ctx_summary_id WHERE n.id=?`, id).
		Scan(&n.ID, &n.Deck, &inAnki, &f[0], &f[1], &f[2], &f[3], &f[4], &f[5], &f[6], &f[7], &f[8], &f[9],
			&n.Summary, &n.ContextLines, &toks, &n.NUnknown, &n.ParsedModel)
	if err != nil {
		return n, err
	}
	n.InAnki = inAnki == 1
	for i, k := range []string{"Sentence", "Sentence Audio", "Word", "Word Audio", "Definition", "Definition Audio", "am-study-morphs", "Image", "Notes", "Context"} {
		n.Fields[k] = f[i]
	}
	var raw [][2]any
	json.Unmarshal([]byte(toks), &raw)
	var ids []int64
	for _, t := range raw {
		if v, ok := t[1].(float64); ok && v > 0 {
			ids = append(ids, int64(v))
		}
	}
	info := map[int64]tokenOut{}
	if len(ids) > 0 {
		rows, _ := db.Query(`SELECT id, base, reading, known, COALESCE(known_src,''), COALESCE(rank,0) FROM morphs WHERE id IN (` + joinIDs(ids) + `)`)
		for rows != nil && rows.Next() {
			var t tokenOut
			rows.Scan(&t.MorphID, &t.Base, &t.Reading, &t.Known, &t.Src, &t.Rank)
			info[t.MorphID] = t
		}
		if rows != nil {
			rows.Close()
		}
	}
	for _, t := range raw {
		s, _ := t[0].(string)
		v, _ := t[1].(float64)
		if v > 0 {
			o := info[int64(v)]
			o.Surface = s
			n.Tokens = append(n.Tokens, o)
		} else {
			n.Tokens = append(n.Tokens, tokenOut{Surface: s, Known: true})
		}
	}
	n.Tags = noteTags(db, id)
	rows, _ := db.Query(`SELECT c.id, c.type, c.queue, c.ivl, COALESCE(d.name,'') FROM cards c LEFT JOIN decks d ON d.id=c.deck_id WHERE c.note_id=?`, id)
	for rows != nil && rows.Next() {
		var cid, typ, q, ivl int64
		var dn string
		rows.Scan(&cid, &typ, &q, &ivl, &dn)
		n.Cards = append(n.Cards, map[string]any{"id": cid, "type": typ, "queue": q, "ivl": ivl, "deck": dn})
	}
	if rows != nil {
		rows.Close()
	}
	return n, nil
}

type wordRow struct {
	ID      int64  `json:"id"`
	Base    string `json:"base"`
	Reading string `json:"reading"`
	Pos     string `json:"pos"`
	Freq    int    `json:"freq"`
	Rank    int    `json:"rank"`
	Known   bool   `json:"known"`
	Src     string `json:"src"`
	IPlus1  int    `json:"i1_cards"`
}

func listWords(db *sql.DB, q, known string, limit, offset int) ([]wordRow, error) {
	where := "freq > 0"
	var args []any
	if q != "" {
		where += " AND (base LIKE ? OR reading LIKE ?)"
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	switch known {
	case "1":
		where += " AND known = 1"
	case "0":
		where += " AND known = 0"
	}
	args = append(args, limit, offset)
	rows, err := db.Query(`SELECT id, base, reading, COALESCE(pos,''), freq, COALESCE(rank,0), known, COALESCE(known_src,''),
	   (SELECT COUNT(*) FROM notes n WHERE n.n_unknown = 1 AND n.unk_morph = morphs.id)
	   FROM morphs WHERE `+where+` ORDER BY rank LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []wordRow{}
	for rows.Next() {
		var w wordRow
		rows.Scan(&w.ID, &w.Base, &w.Reading, &w.Pos, &w.Freq, &w.Rank, &w.Known, &w.Src, &w.IPlus1)
		out = append(out, w)
	}
	return out, nil
}

type tagsetRank struct {
	Rank    int         `json:"rank"`
	Word    string      `json:"word"`
	Reading string      `json:"reading"`
	Cards []searchRow `json:"cards"`
}

func tagsetView(db *sql.DB, prefix string, limit, offset int) ([]tagsetRank, error) {
	var tsid int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE prefix=?`, prefix).Scan(&tsid); err != nil {
		return []tagsetRank{}, nil
	}
	var ranks []int64
	rr, err := db.Query(`SELECT DISTINCT rank FROM tagset_assign WHERE tagset_id=? ORDER BY rank LIMIT ? OFFSET ?`, tsid, limit, offset)
	if err != nil {
		return nil, err
	}
	for rr.Next() {
		var r int64
		rr.Scan(&r)
		ranks = append(ranks, r)
	}
	rr.Close()
	if len(ranks) == 0 {
		return []tagsetRank{}, nil
	}
	rows, err := db.Query(`SELECT rank, word, note_id, tag FROM tagset_assign WHERE tagset_id=? AND rank IN (`+joinIDs(ranks)+`) ORDER BY rank, pos`, tsid)
	if err != nil {
		return nil, err
	}
	type a struct {
		rank int
		word string
		nid  int64
		tag  string
	}
	var as []a
	for rows.Next() {
		var x a
		rows.Scan(&x.rank, &x.word, &x.nid, &x.tag)
		as = append(as, x)
	}
	rows.Close()
	out := []tagsetRank{}
	for _, x := range as {
		if len(out) == 0 || out[len(out)-1].Rank != x.rank {
			var rd string // reading of the word on its best card
			db.QueryRow(`SELECT m.reading FROM notes n JOIN morphs m ON m.id=n.unk_morph WHERE n.id=?`, x.nid).Scan(&rd)
			out = append(out, tagsetRank{Rank: x.rank, Word: x.word, Reading: rd})
		}
		rs, err := search(db, "nid:"+strconv.FormatInt(x.nid, 10), 1, 0)
		if err == nil && len(rs) == 1 {
			rs[0].Tags = []string{x.tag}
			out[len(out)-1].Cards = append(out[len(out)-1].Cards, rs[0])
		}
	}
	return out, nil
}
