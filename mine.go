package main

// Helpers for the /mvj-mine skill: resolve a mining tag to its note (and the
// word's alternates), make sure that note is a real Anki note with the Word
// field pre-filled (what the old *_daily add-ons did), and record the mine.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type noteOut struct {
	Tag           string `json:"tag,omitempty"`
	Pos           int    `json:"pos,omitempty"`
	NoteID        int64  `json:"nid"`
	Word          string `json:"word,omitempty"`
	Deck          string `json:"deck"`
	InAnki        bool   `json:"in_anki"`
	Sentence      string `json:"sentence"`
	SentenceAudio string `json:"sentence_audio"`
	AudioPath     string `json:"audio_path,omitempty"`
	Image         string `json:"image"`
	Translation   string `json:"translation"`
	NUnknown      int    `json:"n_unknown"`
}

var studyDeck = envOr("MDB_STUDY_DECK", "Study Deck")

var reAudio = regexp.MustCompile(`\[(?:audio|sound):([^\]]+)\]`)
var reImg = regexp.MustCompile(`<img[^>]+src="([^"]+)"`)

func mediaPath(name string) string {
	if name == "" {
		return ""
	}
	for _, d := range mediaDirs {
		p := filepath.Join(d, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func audioName(field string) string {
	if m := reAudio.FindStringSubmatch(field); m != nil {
		return m[1]
	}
	return ""
}

func loadNoteOut(db *sql.DB, id int64) (noteOut, error) {
	var o noteOut
	var inAnki int
	err := db.QueryRow(`SELECT n.id, COALESCE(d.name,''), n.in_anki, COALESCE(n.sentence,''), COALESCE(n.sentence_audio,''),
	   COALESCE(n.image,''), COALESCE(n.notes,''), n.n_unknown FROM notes n LEFT JOIN decks d ON d.id=n.deck_id WHERE n.id=?`, id).
		Scan(&o.NoteID, &o.Deck, &inAnki, &o.Sentence, &o.SentenceAudio, &o.Image, &o.Translation, &o.NUnknown)
	o.InAnki = inAnki == 1
	o.AudioPath = mediaPath(audioName(o.SentenceAudio))
	return o, err
}

// alternates: for "PREFIX::NNNN::PP" return that card and every later
// alternate of the same word, in order.
func cmdAlternates(db *sql.DB, tag string) ([]noteOut, error) {
	m := regexp.MustCompile(`^(.+)::(\d+)$`).FindStringSubmatch(tag)
	if m == nil {
		return nil, fmt.Errorf("not a positional mining tag (…::NNNN::PP): %s", tag)
	}
	prefix := m[1]
	var start int
	fmt.Sscan(m[2], &start)
	rows, err := db.Query(`SELECT tag, pos, note_id, word FROM tagset_assign WHERE tag LIKE ? ESCAPE '\' AND pos >= ? ORDER BY pos`,
		likeGlob(prefix)+"::%", start)
	if err != nil {
		return nil, err
	}
	type a struct {
		tag  string
		pos  int
		nid  int64
		word string
	}
	var as []a
	for rows.Next() {
		var x a
		rows.Scan(&x.tag, &x.pos, &x.nid, &x.word)
		as = append(as, x)
	}
	rows.Close()
	var out []noteOut
	for _, x := range as {
		o, err := loadNoteOut(db, x.nid)
		if err != nil {
			continue
		}
		o.Tag, o.Pos, o.Word = x.tag, x.pos, x.word
		out = append(out, o)
	}
	return out, nil
}

// prepareMine gets a database card ready for MvJ's All-In-One and returns the
// Anki note id to run it on:
//   - the pool note is still in Anki -> fill its Word field (what the old
//     *_daily add-ons did); All-In-One then moves it into Study Deck
//   - it only exists in the database -> create it straight in the Study Deck
//     (fields, context, clip + screenshot copied into collection.media) and
//     link it to its pool row
func prepareMine(db *sql.DB, nid int64, word string) (int64, error) {
	writeMu.Lock()
	defer writeMu.Unlock()
	var inAnki int
	var sentence, sAudio, image, notes, wordF, summary, lines string
	err := db.QueryRow(`SELECT n.in_anki, COALESCE(n.sentence,''), COALESCE(n.sentence_audio,''),
	   COALESCE(n.image,''), COALESCE(n.notes,''), COALESCE(n.word,''), COALESCE(s.text,''), COALESCE(n.ctx_lines,'')
	   FROM notes n LEFT JOIN summaries s ON s.id=n.ctx_summary_id WHERE n.id=?`, nid).
		Scan(&inAnki, &sentence, &sAudio, &image, &notes, &wordF, &summary, &lines)
	if err != nil {
		return 0, fmt.Errorf("note %d not in mining-db: %w", nid, err)
	}
	if word == "" {
		word = wordF
	}
	if inAnki == 1 {
		var infos []acNote
		if err := ac("notesInfo", map[string]any{"notes": []int64{nid}}, &infos); err != nil {
			return 0, err
		}
		if len(infos) == 1 && infos[0].NoteID == nid {
			if word != "" && infos[0].Fields["Word"].Value != word {
				if err := ac("updateNoteFields", map[string]any{"note": map[string]any{"id": nid, "fields": map[string]string{"Word": word}}}, nil); err != nil {
					return 0, err
				}
				db.Exec(`UPDATE notes SET word=? WHERE id=?`, word, nid)
			}
			return nid, nil
		}
		db.Exec(`UPDATE notes SET in_anki=0 WHERE id=?`, nid)
	}

	for _, name := range noteMedia(sAudio, image) {
		src := mediaPath(name)
		if src == "" {
			return 0, fmt.Errorf("media %s not found in mining-db/media, collection.media or database.media", name)
		}
		if err := ac("storeMediaFile", map[string]any{"filename": name, "path": src}, nil); err != nil {
			return 0, err
		}
	}
	ctx := ""
	if summary != "" || lines != "" {
		ctx = "Episode summary: " + summary + " | Sentence context: " + lines
	}
	var tags []string
	rows, _ := db.Query(`SELECT tag FROM note_tags WHERE note_id=?`, nid)
	for rows.Next() {
		var t string
		rows.Scan(&t)
		tags = append(tags, t)
	}
	rows.Close()
	var newID int64
	err = ac("addNote", map[string]any{"note": map[string]any{
		"deckName": studyDeck, "modelName": mvjNotetype,
		"fields": map[string]string{"Sentence": sentence, "Sentence Audio": sAudio, "Word": word, "Image": image,
			"Notes": notes, "Context": ctx},
		"tags":    append(tags, "mining-db"),
		"options": map[string]any{"allowDuplicate": true},
	}}, &newID)
	if err != nil {
		return 0, err
	}
	db.Exec(`INSERT OR REPLACE INTO study_links(study_id, source_id, at) VALUES(?,?,?)`, newID, nid, time.Now().Unix())
	return newID, nil
}

func imgName(field string) string {
	if m := reImg.FindStringSubmatch(field); m != nil {
		return m[1]
	}
	return ""
}

func recordMined(db *sql.DB, nid int64, tag, word string) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	_, err := db.Exec(`INSERT INTO mined(note_id, tag, word, at) VALUES(?,?,?,?)`, nid, tag, word, time.Now().Unix())
	return err
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	enc.Encode(v)
}

func wordOfTag(db *sql.DB, tag string) string {
	var w string
	db.QueryRow(`SELECT word FROM tagset_assign WHERE tag=?`, strings.TrimSpace(tag)).Scan(&w)
	return w
}
