package main

// The database's own media store and direct subs2srs .apkg import, so new
// decks never have to go through Anki.

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// noteMedia lists the media files a note references.
func noteMedia(fields ...string) []string {
	var out []string
	for _, f := range fields {
		for _, m := range reAudioAll.FindAllStringSubmatch(f, -1) {
			out = append(out, m[1])
		}
		for _, m := range reImgAll.FindAllStringSubmatch(f, -1) {
			out = append(out, unescapeSrc(m[1]))
		}
	}
	return out
}

var (
	reAudioAll = regexp.MustCompile(`\[(?:audio|sound):([^\]]+)\]`)
	reImgAll   = regexp.MustCompile(`<img[^>]+src="([^"]+)"`)
)

// cmdMediaAdopt hard-links every clip/screenshot that a pool note uses into
// the database's media store. Same disk, so it costs no space, and the files
// survive if the Anki-side folders are cleaned or the MvJ add-on removed.
func cmdMediaAdopt(db *sql.DB) error {
	if err := os.MkdirAll(mediaStore, 0o755); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT COALESCE(sentence_audio,''), COALESCE(image,''), COALESCE(word_audio,''), COALESCE(definition_audio,'') FROM notes`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var a, i, wa, da string
		rows.Scan(&a, &i, &wa, &da)
		names = append(names, noteMedia(a, i, wa, da)...)
	}
	rows.Close()
	t0 := time.Now()
	linked, have, copied, missing := 0, 0, 0, 0
	var missingEx []string
	for _, n := range names {
		n = filepath.Base(n)
		dst := filepath.Join(mediaStore, n)
		if _, err := os.Stat(dst); err == nil {
			have++
			continue
		}
		src := ""
		for _, d := range mediaDirs[1:] {
			for _, cand := range []string{n, norm.NFC.String(n), norm.NFD.String(n)} {
				p := filepath.Join(d, cand)
				if _, err := os.Stat(p); err == nil {
					src = p
					break
				}
			}
			if src != "" {
				break
			}
		}
		if src == "" {
			missing++
			if len(missingEx) < 5 {
				missingEx = append(missingEx, n)
			}
			continue
		}
		if err := os.Link(src, dst); err != nil {
			if err := copyFile(src, dst); err != nil {
				return err
			}
			copied++
		} else {
			linked++
		}
		if (linked+copied)%200000 == 0 && linked+copied > 0 {
			log.Printf("media: %d linked…", linked+copied)
		}
	}
	log.Printf("media store %s: %d linked, %d copied, %d already there, %d missing everywhere %v (%s)",
		mediaStore, linked, copied, have, missing, missingEx, time.Since(t0).Round(time.Second))
	setMeta(db, "media_missing", fmt.Sprint(missing))
	return nil
}

// unescapeSrc decodes a URL-encoded <img src> (Anki encodes # and spaces).
func unescapeSrc(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst + ".part")
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Rename(dst+".part", dst)
}

// ---------------------------------------------------------------------------
// .apkg import

// fieldRole maps a subs2srs / MvJ field name to a notes column.
func fieldRole(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "text", "sentence", "expression", "japanese":
		return "sentence"
	case "audio", "sentence audio", "sound", "snapshot audio":
		return "sentence_audio"
	case "image", "snapshot", "picture", "screenshot":
		return "image"
	case "context":
		return "context"
	case "notes", "meaning", "translation", "english":
		return "notes"
	case "word":
		return "word"
	}
	return ""
}

type apkgModel struct {
	name   string
	fields []string
}

func cmdImportApkg(db *sql.DB, path, deckOverride string) error {
	t0 := time.Now()
	zr, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer zr.Close()
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dbPath), "apkg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	colPath := filepath.Join(tmp, "collection.db")
	switch {
	case files["collection.anki21b"] != nil:
		zst := filepath.Join(tmp, "c.zst")
		if err := extract(files["collection.anki21b"], zst); err != nil {
			return err
		}
		if out, err := exec.Command("zstd", "-q", "-d", zst, "-o", colPath).CombinedOutput(); err != nil {
			return fmt.Errorf("zstd: %v %s", err, out)
		}
	case files["collection.anki21"] != nil:
		err = extract(files["collection.anki21"], colPath)
	case files["collection.anki2"] != nil:
		err = extract(files["collection.anki2"], colPath)
	default:
		return fmt.Errorf("%s: no collection inside", path)
	}
	if err != nil {
		return err
	}
	col, err := sql.Open("sqlite3_anki", "file:"+colPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer col.Close()

	models := map[int64]apkgModel{}
	var modelsJSON string
	col.QueryRow(`SELECT models FROM col`).Scan(&modelsJSON)
	if modelsJSON != "" && modelsJSON != "{}" {
		var m map[string]struct {
			Name string `json:"name"`
			Flds []struct {
				Name string `json:"name"`
				Ord  int    `json:"ord"`
			} `json:"flds"`
		}
		json.Unmarshal([]byte(modelsJSON), &m)
		for id, v := range m {
			var mid int64
			fmt.Sscan(id, &mid)
			fl := make([]string, len(v.Flds))
			for _, f := range v.Flds {
				if f.Ord < len(fl) {
					fl[f.Ord] = f.Name
				}
			}
			models[mid] = apkgModel{v.Name, fl}
		}
	} else { // newer schema
		rows, err := col.Query(`SELECT nt.id, nt.name, f.ord, f.name FROM notetypes nt JOIN fields f ON f.ntid = nt.id ORDER BY nt.id, f.ord`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			var nn, fn string
			var ord int
			rows.Scan(&id, &nn, &ord, &fn)
			m := models[id]
			m.name = nn
			m.fields = append(m.fields, fn)
			models[id] = m
		}
		rows.Close()
	}

	// deck name: the apkg's non-Default deck, filed under Database::
	deckName := deckOverride
	if deckName == "" {
		var decksJSON string
		col.QueryRow(`SELECT decks FROM col`).Scan(&decksJSON)
		var names []string
		if decksJSON != "" && decksJSON != "{}" {
			var d map[string]struct {
				Name string `json:"name"`
			}
			json.Unmarshal([]byte(decksJSON), &d)
			for _, v := range d {
				names = append(names, v.Name)
			}
		} else {
			rows, _ := col.Query(`SELECT name FROM decks`)
			for rows != nil && rows.Next() {
				var n string
				rows.Scan(&n)
				names = append(names, strings.ReplaceAll(n, "\x1f", "::"))
			}
			if rows != nil {
				rows.Close()
			}
		}
		for _, n := range names {
			if n != "Default" {
				deckName = n
			}
		}
		if deckName == "" {
			deckName = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		if !strings.HasPrefix(deckName, "Database::") {
			deckName = "Database::" + deckName
		}
	}
	var deckID int64
	if err := db.QueryRow(`SELECT id FROM decks WHERE name=?`, deckName).Scan(&deckID); err != nil {
		deckID = time.Now().UnixMilli()
		if _, err := db.Exec(`INSERT INTO decks(id, name, is_pool) VALUES(?,?,?)`, deckID, deckName, b2i(strings.HasPrefix(deckName, "Database::"))); err != nil {
			return err
		}
	}

	// media: legacy JSON map {"0": "name.mp3"}
	os.MkdirAll(mediaStore, 0o755)
	nMedia := 0
	if mf := files["media"]; mf != nil {
		rc, _ := mf.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		var mm map[string]string
		if json.Unmarshal(b, &mm) != nil {
			return fmt.Errorf("media map is not JSON (newer .apkg format) — export with legacy media or let Claude add protobuf support")
		}
		for idx, name := range mm {
			zf := files[idx]
			if zf == nil {
				continue
			}
			dst := filepath.Join(mediaStore, filepath.Base(norm.NFC.String(name)))
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			if err := extract(zf, dst); err != nil {
				return err
			}
			nMedia++
		}
	}

	// notes
	rows, err := col.Query(`SELECT id, guid, mid, mod, tags, flds FROM notes ORDER BY id`)
	if err != nil {
		return err
	}
	type in struct {
		id, mid, mod int64
		guid, tags   string
		flds         []string
	}
	var notes []in
	for rows.Next() {
		var n in
		var flds string
		rows.Scan(&n.id, &n.guid, &n.mid, &n.mod, &n.tags, &flds)
		n.flds = strings.Split(flds, "\x1f")
		notes = append(notes, n)
	}
	rows.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	sums := map[string]int64{}
	var ids []int64
	added, updated := 0, 0
	nextID := time.Now().UnixMilli()
	for _, n := range notes {
		m := models[n.mid]
		v := map[string]string{}
		for i, fname := range m.fields {
			if i < len(n.flds) {
				if role := fieldRole(fname); role != "" && v[role] == "" {
					v[role] = n.flds[i]
				}
			}
		}
		v["sentence_audio"] = strings.ReplaceAll(v["sentence_audio"], "[sound:", "[audio:") // MvJ format
		summary, lines := splitContext(v["context"])
		var sid any
		if summary != "" {
			s, ok := sums[summary]
			if !ok {
				tx.QueryRow(`SELECT id FROM summaries WHERE text=?`, summary).Scan(&s)
				if s == 0 {
					res, err := tx.Exec(`INSERT INTO summaries(text) VALUES(?)`, summary)
					if err != nil {
						tx.Rollback()
						return err
					}
					s, _ = res.LastInsertId()
				}
				sums[summary] = s
			}
			sid = s
		}
		// same guid already here -> update; id clash with another note -> new id
		var existing int64
		tx.QueryRow(`SELECT id FROM notes WHERE guid=?`, n.guid).Scan(&existing)
		id := n.id
		if existing != 0 {
			id = existing
			updated++
		} else {
			var clash int
			tx.QueryRow(`SELECT COUNT(*) FROM notes WHERE id=?`, id).Scan(&clash)
			if clash > 0 {
				nextID++
				id = nextID
			}
			added++
		}
		_, err := tx.Exec(`INSERT INTO notes(id, guid, deck_id, mod, in_anki, sentence, sentence_audio, word, image, notes, context,
		   ctx_summary_id, ctx_lines, parse_field) VALUES(?,?,?,?,0,?,?,?,?,?,'',?,?,'Sentence')
		   ON CONFLICT(id) DO UPDATE SET deck_id=excluded.deck_id, mod=excluded.mod, sentence=excluded.sentence,
		   sentence_audio=excluded.sentence_audio, image=excluded.image, notes=excluded.notes,
		   ctx_summary_id=excluded.ctx_summary_id, ctx_lines=excluded.ctx_lines`,
			id, n.guid, deckID, n.mod, v["sentence"], v["sentence_audio"], v["word"], v["image"], v["notes"], sid, lines)
		if err != nil {
			tx.Rollback()
			return err
		}
		for _, t := range strings.Fields(n.tags) {
			tx.Exec(`INSERT OR IGNORE INTO note_tags(note_id, tag) VALUES(?,?)`, id, t)
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("import-apkg %s → %s: %d new, %d updated notes, %d media files", filepath.Base(path), deckName, added, updated, nMedia)

	// tokenize with the reading model + incremental recalc
	writeMu.Lock()
	defer writeMu.Unlock()
	touched, err := parseNotes(db, ids)
	if err != nil {
		return err
	}
	flipped, err := refreshKnown(db, keys(touched))
	if err != nil {
		return err
	}
	_ = flipped
	if _, err := refreshNotesByID(db, ids); err != nil {
		return err
	}
	if _, err := refreshNotes(db, flipped); err != nil {
		return err
	}
	if err := refreshPriority(db); err != nil {
		return err
	}
	if err := computeTagsets(db); err != nil {
		return err
	}
	var i0, i1, i2 int
	db.QueryRow(`SELECT SUM(n_unknown=0), SUM(n_unknown=1), SUM(n_unknown>=2) FROM notes WHERE deck_id=?`, deckID).Scan(&i0, &i1, &i2)
	log.Printf("%s: i+0 %d · i+1 %d · i+≥2 %d (done in %s)", deckName, i0, i1, i2, time.Since(t0).Round(time.Second))
	resetSharedWorker()
	return nil
}

func extract(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
