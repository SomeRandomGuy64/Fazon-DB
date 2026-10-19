package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var ankiConnectURL = envOr("MDB_ANKICONNECT", "http://127.0.0.1:8765")

var ankiHTTP = &http.Client{Timeout: 120 * time.Second}

// ac calls AnkiConnect and decodes "result" into out (may be nil).
func ac(action string, params any, out any) error {
	body := map[string]any{"action": action, "version": 6}
	if params != nil {
		body["params"] = params
	}
	b, _ := json.Marshal(body)
	resp, err := ankiHTTP.Post(ankiConnectURL, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("AnkiConnect unreachable (is Anki open?): %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *string         `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("AnkiConnect %s: %s", action, *r.Error)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type acNote struct {
	NoteID    int64                     `json:"noteId"`
	ModelName string                    `json:"modelName"`
	Tags      []string                  `json:"tags"`
	Fields    map[string]acField        `json:"fields"`
	Cards     []int64                   `json:"cards"`
	Mod       int64                     `json:"mod"`
}

type acField struct {
	Value string `json:"value"`
	Order int    `json:"order"`
}

type acCard struct {
	CardID   int64  `json:"cardId"`
	Note     int64  `json:"note"`
	DeckName string `json:"deckName"`
	Interval int64  `json:"interval"`
	Type     int64  `json:"type"`
	Queue    int64  `json:"queue"`
	Mod      int64  `json:"mod"`
	Ord      int    `json:"ord"`
}

func chunked(ids []int64, n int, f func([]int64) error) error {
	for i := 0; i < len(ids); i += n {
		j := i + n
		if j > len(ids) {
			j = len(ids)
		}
		if err := f(ids[i:j]); err != nil {
			return err
		}
	}
	return nil
}
