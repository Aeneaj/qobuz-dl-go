package downloader

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}

	if db.has("abc") {
		t.Error("expected has(abc) == false on empty DB")
	}

	db.add("abc")
	db.add("def")

	if !db.has("abc") {
		t.Error("expected has(abc) == true after add")
	}
	if !db.has("def") {
		t.Error("expected has(def) == true after add")
	}
	if db.has("xyz") {
		t.Error("expected has(xyz) == false")
	}

	// Adding the same ID twice must not duplicate entries
	db.add("abc")
	data, _ := os.ReadFile(path)
	count := 0
	for _, line := range splitLines(string(data)) {
		if line == "abc" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence of 'abc' in DB file, got %d", count)
	}

	// Re-opening the DB must reload persisted IDs
	db2, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB reload: %v", err)
	}
	if !db2.has("abc") || !db2.has("def") {
		t.Error("reloaded DB missing persisted IDs")
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) && s[start:] != "" {
		out = append(out, s[start:])
	}
	return out
}

func TestDownloadDBNonExistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.db")

	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB on non-existent file: %v", err)
	}
	if db.has("anything") {
		t.Error("fresh DB should not have any IDs")
	}
}
