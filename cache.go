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
