package main

// Live parsing for the /mvj-mine bridge: MvJ's furigana and Word readings come
// from the same warm parser and database instead of MvJ's MeCab.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// liveTok = [surface, offset, base, base_reading, surface_reading, changed]
func (w *worker) parseLive(text string) ([][]any, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, _ := json.Marshal(map[string]string{"live": text})
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	resp, err := w.out.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var r struct {
		Toks [][]any `json:"toks"`
	}
	if err := json.Unmarshal([]byte(resp), &r); err != nil {
		return nil, err
	}
	return r.Toks, nil
}

// liveParse tokenizes text as given (offsets match it) with the shared worker.
func liveParse(text string) ([][]any, error) {
	wk, err := sharedWorker()
	if err != nil {
		return nil, err
	}
	toks, err := wk.parseLive(text)
	if err != nil {
		resetSharedWorker()
		return nil, err
	}
	if toks == nil {
		toks = [][]any{}
	}
	return toks, nil
}

// wordReading is the reading the database parse gave WORD in note NID: the
// (base, reading) the tag sets targeted. Prefers the note's one unknown word.
func wordReading(db *sql.DB, nid int64, word string) (string, error) {
	var tj string
	var unk sql.NullInt64
	err := db.QueryRow(`SELECT COALESCE(tokens,'[]'), unk_morph FROM notes WHERE id=?`, nid).Scan(&tj, &unk)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("note %d not in mining-db", nid)
	}
	if err != nil {
		return "", err
	}
	var toks [][]any
	json.Unmarshal([]byte(tj), &toks)
	var ids []int64
	for _, t := range toks {
		if len(t) == 2 {
			if m, ok := t[1].(float64); ok && m > 0 {
				ids = append(ids, int64(m))
			}
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	rows, err := db.Query(`SELECT id, reading FROM morphs WHERE base=? AND id IN (`+joinIDs(ids)+`)`, word)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	reading := ""
	for rows.Next() {
		var id int64
		var r string
		rows.Scan(&id, &r)
		if unk.Valid && id == unk.Int64 {
			return r, nil
		}
		if reading == "" {
			reading = r
		}
	}
	return reading, nil
}

func registerLiveRoutes(mux *http.ServeMux, db *sql.DB) {
	mux.HandleFunc("/api/parse", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", 405)
			return
		}
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		toks, err := liveParse(req.Text)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"toks": toks})
	})
	mux.HandleFunc("/api/word-reading", func(w http.ResponseWriter, r *http.Request) {
		nid, _ := strconv.ParseInt(r.URL.Query().Get("nid"), 10, 64)
		rd, err := wordReading(db, nid, strings.TrimSpace(r.URL.Query().Get("word")))
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"reading": rd})
	})
}
