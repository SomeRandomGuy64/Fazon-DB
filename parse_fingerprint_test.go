package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParserFingerprintIncludesAnalyzerAndVariant(t *testing.T) {
	oldParser, oldModel, oldWorker := parserDir, modelDir, workerPy
	t.Cleanup(func() { parserDir, modelDir, workerPy = oldParser, oldModel, oldWorker })
	parserDir = t.TempDir()
	modelDir = filepath.Join(parserDir, "models", "best")
	variant := filepath.Join(parserDir, "models", "variant")
	workerPy = filepath.Join(parserDir, "worker.py")
	for _, dir := range []string{modelDir, variant} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, text string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(modelDir, "rd_config.json"), `{"variant_model":"models/variant"}`)
	write(filepath.Join(parserDir, "analyzers.py"), "version 1")
	write(filepath.Join(variant, "extras.json"), `{}`)
	t.Setenv("MDB_PARSER_BACKEND", "sudachi")
	first := modelFingerprint()
	if modelFingerprint() != first {
		t.Fatal("unchanged parser fingerprint is unstable")
	}
	write(filepath.Join(parserDir, "analyzers.py"), "version 2")
	second := modelFingerprint()
	if first == second {
		t.Fatal("analyzer change did not invalidate parses")
	}
	write(filepath.Join(variant, "extras.json"), `{"extra":[]}`)
	third := modelFingerprint()
	if second == third {
		t.Fatal("variant model change did not invalidate parses")
	}
	t.Setenv("MDB_PARSER_BACKEND", "unidic")
	if third == modelFingerprint() {
		t.Fatal("backend change did not invalidate parses")
	}
}
