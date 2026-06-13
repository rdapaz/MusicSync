package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Library cache — persists probed track data to SQLite so subsequent runs
// skip the scan+probe phase entirely.
// ---------------------------------------------------------------------------

const cacheDBName = "musicsync.db"

func cacheDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".musicsync")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, cacheDBName), nil
}

func openCacheDB() (*sql.DB, error) {
	path, err := cacheDBPath()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS source_dirs (
			dir TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS tracks (
			src_path     TEXT PRIMARY KEY,
			title        TEXT,
			artist       TEXT,
			album_artist TEXT,
			album        TEXT,
			genre        TEXT,
			date         TEXT,
			composer     TEXT,
			track_num    INTEGER,
			disc         INTEGER,
			size         INTEGER,
			bucket       TEXT,
			album_key    TEXT
		);
		CREATE TABLE IF NOT EXISTS collections (
			name     TEXT NOT NULL,
			src_path TEXT NOT NULL,
			PRIMARY KEY (name, src_path)
		);
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return db, nil
}

// SaveLibraryCache writes the library and source dirs to the SQLite cache.
func SaveLibraryCache(sourceDirs []string, lib *Library) error {
	db, err := openCacheDB()
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clear old data.
	if _, err := tx.Exec("DELETE FROM source_dirs"); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tracks"); err != nil {
		return err
	}

	// Insert source dirs.
	for _, d := range sourceDirs {
		if _, err := tx.Exec("INSERT INTO source_dirs (dir) VALUES (?)", d); err != nil {
			return err
		}
	}

	// Insert tracks.
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO tracks
		(src_path, title, artist, album_artist, album, genre, date, composer, track_num, disc, size, bucket, album_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, t := range lib.Tracks {
		if _, err := stmt.Exec(
			t.SrcPath, t.Title, t.Artist, t.AlbumArtist, t.Album,
			t.Genre, t.Date, t.Composer, t.TrackNum, t.Disc, t.Size,
			t.Bucket, t.AlbumKey,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LoadLibraryCache loads the cached library if it exists and the source dirs match.
// Returns nil, nil if no valid cache is available.
func LoadLibraryCache(sourceDirs []string) (*Library, error) {
	dbPath, err := cacheDBPath()
	if err != nil {
		return nil, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil // no cache file
	}

	db, err := openCacheDB()
	if err != nil {
		return nil, nil
	}
	defer db.Close()

	// Check source dirs match.
	rows, err := db.Query("SELECT dir FROM source_dirs ORDER BY dir")
	if err != nil {
		return nil, nil
	}
	var cachedDirs []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, nil
		}
		cachedDirs = append(cachedDirs, d)
	}
	rows.Close()

	if !dirsMatch(cachedDirs, sourceDirs) {
		return nil, nil
	}

	// Load tracks.
	trows, err := db.Query(`SELECT src_path, title, artist, album_artist, album,
		genre, date, composer, track_num, disc, size, bucket, album_key FROM tracks`)
	if err != nil {
		return nil, nil
	}
	defer trows.Close()

	var tracks []*Track
	for trows.Next() {
		t := &Track{}
		if err := trows.Scan(
			&t.SrcPath, &t.Title, &t.Artist, &t.AlbumArtist, &t.Album,
			&t.Genre, &t.Date, &t.Composer, &t.TrackNum, &t.Disc, &t.Size,
			&t.Bucket, &t.AlbumKey,
		); err != nil {
			return nil, nil
		}
		tracks = append(tracks, t)
	}

	if len(tracks) == 0 {
		return nil, nil
	}

	lib := buildLibrary(tracks)
	return lib, nil
}

func dirsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := make([]string, len(a))
	sb := make([]string, len(b))
	copy(sa, a)
	copy(sb, b)
	sort.Strings(sa)
	sort.Strings(sb)
	return strings.Join(sa, "\n") == strings.Join(sb, "\n")
}

// ---------------------------------------------------------------------------
// Named collections — save and restore multiple named sets of picked tracks,
// so a run can be resumed (or a saved set re-used) after an interruption.
// ---------------------------------------------------------------------------

// LastCopyCollection is the reserved name under which the current selection is
// auto-saved when a copy starts, so an interrupted run can be reloaded.
const LastCopyCollection = "Last copy"

// SaveCollection replaces the named collection with the given source paths.
func SaveCollection(name string, srcPaths []string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("collection name is empty")
	}
	db, err := openCacheDB()
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM collections WHERE name = ?", name); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT OR IGNORE INTO collections (name, src_path) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range srcPaths {
		if _, err := stmt.Exec(name, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadCollection returns the source paths of a named collection as a set.
func LoadCollection(name string) (map[string]bool, error) {
	db, err := openCacheDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT src_path FROM collections WHERE name = ?", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	paths := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths[p] = true
	}
	return paths, nil
}

// ListCollections returns the saved collection names, sorted. It does not
// create the cache DB if none exists yet (returns an empty list instead).
func ListCollections() ([]string, error) {
	dbPath, err := cacheDBPath()
	if err != nil {
		return nil, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil // no cache yet
	}

	db, err := openCacheDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query("SELECT DISTINCT name FROM collections ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, nil
}

// DeleteCollection removes a named collection.
func DeleteCollection(name string) error {
	db, err := openCacheDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("DELETE FROM collections WHERE name = ?", name)
	return err
}
