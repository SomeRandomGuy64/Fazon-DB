package main

import (
	"database/sql"
	"testing"
)

func TestNoteParseWindowKeepsDeckContext(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY,deck_id INTEGER,parse_field TEXT,sentence TEXT,word TEXT);
 INSERT INTO notes VALUES(1,10,'Sentence','previous',''),(2,20,'Sentence','other deck',''),(3,10,'Sentence','target',''),(4,20,'Sentence','other deck next',''),(5,10,'Sentence','next','');`)
	if err != nil {
		t.Fatal(err)
	}
	lines, text, sentence, err := noteParseWindow(db, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].ID != 1 || lines[1].ID != 3 || lines[2].ID != 5 {
		t.Fatalf("wrong context: %+v", lines)
	}
	if text != "target" || sentence != "target" {
		t.Fatalf("wrong target: %q %q", text, sentence)
	}
	lines, _, _, err = noteParseWindow(db, 1)
	if err != nil || len(lines) != 2 || lines[0].ID != 1 || lines[1].ID != 3 {
		t.Fatalf("wrong start-of-deck context: %+v %v", lines, err)
	}
	lines, _, _, err = noteParseWindow(db, 5)
	if err != nil || len(lines) != 2 || lines[0].ID != 3 || lines[1].ID != 5 {
		t.Fatalf("wrong end-of-deck context: %+v %v", lines, err)
	}
}
