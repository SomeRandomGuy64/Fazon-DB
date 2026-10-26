package main

// A small Anki-like search language used by the CLI and the web harness.
//
//   deck:Database::higuchi_*   deck glob (subdecks included)
//   tag:!higuchi-top2000::0012 tag (exact or its children; * wildcard). Covers
//                              imported tags, tag-set tags and the derived
//                              _card-status::i+0 / i+1 / i+≥2
//   i:1  i:0  i:2+             unknown-word count
//   word:分かる                note contains this word (dictionary form)
//   unk:分かる                 this word is unknown on the note
//   nid:123,456                note ids
//   pool:1 / pool:0            subs2srs pool decks or not
//   anki:0                     notes no longer in Anki
//   susp:0 / susp:1            without / with a suspended (excluded) unknown word
//   -term                      negate any term
//   anything else              substring of the sentence

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

func tokenizeQuery(q string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range q {
		switch {
		case r == '"':
			inQ = !inQ
		case (r == ' ' || r == '　') && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func likeGlob(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
	return strings.ReplaceAll(s, "*", "%")
}

// buildWhere compiles a query to a SQL condition over alias n (notes).
func buildWhere(db *sql.DB, q string) (string, []any, error) {
	var conds []string
	var args []any
	for _, t := range tokenizeQuery(q) {
		neg := false
		if strings.HasPrefix(t, "-") && len(t) > 1 {
			neg, t = true, t[1:]
		}
		key, val, hasKey := strings.Cut(t, ":")
		if !hasKey || !isKey(key) {
			key, val = "", t
		}
		var c string
		switch strings.ToLower(key) {
		case "deck":
			ids, err := decksMatching(db, val)
			if err != nil {
				return "", nil, err
			}
			if len(ids) == 0 {
				c = "0"
			} else {
				c = "n.deck_id IN (" + joinIDs(ids) + ")"
			}
		case "tag":
			c, args = tagCond(val, args)
		case "i":
			switch {
			case strings.HasSuffix(val, "+"):
				n, _ := strconv.Atoi(strings.TrimSuffix(val, "+"))
				c = fmt.Sprintf("n.n_unknown >= %d", n)
			default:
				n, err := strconv.Atoi(val)
				if err != nil {
					return "", nil, fmt.Errorf("bad i:%s", val)
				}
				c = fmt.Sprintf("n.n_unknown = %d", n)
			}
		case "word":
			c = "n.id IN (SELECT nm.note_id FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE m.base = ?)"
			args = append(args, val)
		case "unk":
			c = "n.id IN (SELECT nm.note_id FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE m.base = ? AND m.known = 0 AND m.ignored = 0)"
			args = append(args, val)
		case "nid":
			var ids []int64
			for _, s := range strings.Split(val, ",") {
				if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					ids = append(ids, id)
				}
			}
			c = "n.id IN (" + joinIDs(ids) + ")"
			if len(ids) == 0 {
				c = "0"
			}
		case "pool":
			c = "n.deck_id IN (SELECT id FROM decks WHERE is_pool = " + boolInt(val) + ")"
		case "anki":
			c = "n.in_anki = " + boolInt(val)
		case "susp": // susp:1 = has an unknown word that is suspended and not allowed (susp:0 = none)
			c = "EXISTS (SELECT 1 FROM note_morphs nm JOIN morphs m ON m.id=nm.morph_id WHERE nm.note_id=n.id AND m.known = 0 AND nm.morph_id IN (" + excludedSQL + "))"
			if boolInt(val) == "0" {
				c = "NOT " + c
			}
		default:
			c = "n.sentence LIKE ? ESCAPE '\\'"
			args = append(args, "%"+likeGlob(val)+"%")
		}
		if neg {
			c = "NOT (" + c + ")"
		}
		conds = append(conds, c)
	}
	if len(conds) == 0 {
		return "1", args, nil
	}
	return strings.Join(conds, " AND "), args, nil
}

func isKey(k string) bool {
	switch strings.ToLower(k) {
	case "deck", "tag", "i", "word", "unk", "nid", "pool", "anki", "susp":
		return true
	}
	return false
}

func boolInt(v string) string {
	if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") {
		return "0"
	}
	return "1"
}

func tagCond(val string, args []any) (string, []any) {
	// derived AnkiMorphs status tags
	switch strings.TrimPrefix(val, "_card-status::") {
	case "i+0":
		return "n.n_unknown = 0", args
	case "i+1":
		return "n.n_unknown = 1", args
	case "i+≥2", "i+>=2":
		return "n.n_unknown >= 2", args
	}
	g := likeGlob(val)
	c := `(n.id IN (SELECT note_id FROM note_tags WHERE tag LIKE ? ESCAPE '\' OR tag LIKE ? ESCAPE '\')
	   OR n.id IN (SELECT note_id FROM tagset_assign WHERE tag LIKE ? ESCAPE '\' OR tag LIKE ? ESCAPE '\'))`
	return c, append(args, g, g+"::%", g, g+"::%")
}

func decksMatching(db *sql.DB, glob string) ([]int64, error) {
	rows, err := db.Query(`SELECT id, name FROM decks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	re := deckGlob(glob)
	var out []int64
	for rows.Next() {
		var id int64
		var n string
		rows.Scan(&id, &n)
		if re.MatchString(n) {
			out = append(out, id)
		}
	}
	return out, nil
}
