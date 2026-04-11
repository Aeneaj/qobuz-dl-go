package downloader

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// downloadDB tracks successfully downloaded track IDs in a plain-text file
// (one ID per line). It avoids re-downloading when files have been moved or
// renamed, complementing the path-based check in downloadAndTag.
type downloadDB struct {
	mu   sync.Mutex
	path string
	ids  map[string]struct{}
}

// openDB loads (or creates) the downloads database at the given path.
func openDB(path string) (*downloadDB, error) {
	db := &downloadDB{
		path: path,
		ids:  make(map[string]struct{}),
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return db, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			db.ids[id] = struct{}{}
		}
	}
	return db, sc.Err()
}

// has reports whether the track ID was previously recorded.
func (db *downloadDB) has(id string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, ok := db.ids[id]
	return ok
}

// add records a track ID in memory and persists it to disk.
// The lock is released before file I/O so concurrent downloads are not
// serialised while waiting for the filesystem.
func (db *downloadDB) add(id string) error {
	db.mu.Lock()
	if _, exists := db.ids[id]; exists {
		db.mu.Unlock()
		return nil
	}
	db.ids[id] = struct{}{}
	db.mu.Unlock()

	f, err := os.OpenFile(db.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		// Roll back the in-memory entry so the track is retried on next run.
		db.mu.Lock()
		delete(db.ids, id)
		db.mu.Unlock()
		return fmt.Errorf("open downloads DB: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(id + "\n"); err != nil {
		db.mu.Lock()
		delete(db.ids, id)
		db.mu.Unlock()
		return fmt.Errorf("write downloads DB: %w", err)
	}
	return nil
}
