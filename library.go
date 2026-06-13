package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Audio extensions recognised by the scanner
// ---------------------------------------------------------------------------

var audioExts = map[string]bool{
	".flac": true, ".wav": true, ".aiff": true, ".aif": true,
	".alac": true, ".m4a": true, ".mp3": true, ".ogg": true, ".opus": true,
	".wv": true, ".ape": true,
}

// ---------------------------------------------------------------------------
// Data model
// ---------------------------------------------------------------------------

type Track struct {
	SrcPath     string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Genre       string
	Date        string
	Composer    string
	TrackNum    int
	Disc        int
	Size        int64
	Bucket      string
	AlbumKey    string
	DstDir      string
	DstPath     string
}

// Library holds all probed tracks and a hierarchical index for the tree.
// Tree structure: bucket -> artist -> album -> disc -> []*Track
type Library struct {
	Tracks    []*Track
	Tree      map[string]map[string]map[string]map[string][]*Track
	TotalSize int64
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

// ScanSources walks each directory in dirs and returns paths to audio files.
// progressFn is called periodically with the count of files found so far.
func ScanSources(ctx context.Context, dirs []string, progressFn func(found int)) ([]string, error) {
	var files []string
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("source not accessible: %s: %w", dir, err)
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil // skip unreadable entries
			}
			if d.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if audioExts[ext] {
				files = append(files, path)
				if progressFn != nil && len(files)%100 == 0 {
					progressFn(len(files))
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	sort.Strings(files)
	if progressFn != nil {
		progressFn(len(files))
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// Probing (ffprobe)
// ---------------------------------------------------------------------------

type ffprobeOutput struct {
	Streams []struct {
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		Tags map[string]string `json:"tags"`
	} `json:"format"`
}

var (
	reLeadingNum = regexp.MustCompile(`^\s*([0-9]{1,4})\s*`)
	bufferPool   = sync.Pool{New: func() interface{} { return new(bytes.Buffer) }}
)

// ProbeFiles runs ffprobe concurrently over paths and returns a fully-indexed Library.
func ProbeFiles(ctx context.Context, paths []string, ffprobePath string, workers int, progressFn func(done, total int)) (*Library, error) {
	if workers < 1 {
		workers = 1
	}
	type result struct {
		track *Track
		err   error
		path  string
	}

	jobs := make(chan string, workers*2)
	results := make(chan result, workers*2)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range jobs {
				if ctx.Err() != nil {
					return
				}
				t, err := probeOne(ffprobePath, path)
				results <- result{track: t, err: err, path: path}
			}
		}()
	}

	go func() {
		for _, p := range paths {
			select {
			case jobs <- p:
			case <-ctx.Done():
				break
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	tracks := make([]*Track, 0, len(paths))
	var done uint64
	total := len(paths)

	for r := range results {
		if r.err != nil {
			// skip files that fail to probe
			continue
		}
		tracks = append(tracks, r.track)
		n := atomic.AddUint64(&done, 1)
		if progressFn != nil && (n%50 == 0 || int(n) == total) {
			progressFn(int(n), total)
		}
	}

	lib := buildLibrary(tracks)
	return lib, nil
}

func probeOne(ffprobePath, path string) (*Track, error) {
	args := []string{
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "format_tags:stream_tags",
		"-of", "json",
		path,
	}
	cmd := exec.Command(ffprobePath, args...)
	stdout := bufferPool.Get().(*bytes.Buffer)
	stderr := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		stdout.Reset()
		stderr.Reset()
		bufferPool.Put(stdout)
		bufferPool.Put(stderr)
	}()
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var p ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	// Merge tags: format-level first, then stream-level as fallback.
	tags := map[string]string{}
	for k, v := range p.Format.Tags {
		tags[k] = v
	}
	if len(p.Streams) > 0 {
		for k, v := range p.Streams[0].Tags {
			if _, ok := tags[k]; !ok {
				tags[k] = v
			}
		}
	}

	get := func(keys ...string) string {
		for _, k := range keys {
			for kk, vv := range tags {
				if strings.EqualFold(kk, k) {
					return vv
				}
			}
		}
		return ""
	}

	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	t := &Track{
		SrcPath:     path,
		Title:       strings.TrimSpace(get("TITLE", "title")),
		Artist:      strings.TrimSpace(get("ARTIST", "artist")),
		AlbumArtist: strings.TrimSpace(get("ALBUMARTIST", "albumartist", "ALBUM ARTIST", "album artist", "album_artist")),
		Album:       strings.TrimSpace(get("ALBUM", "album")),
		Genre:       strings.TrimSpace(get("GENRE", "genre")),
		Date:        strings.TrimSpace(firstNonEmpty(get("DATE", "date"), get("YEAR", "year"))),
		Composer:    strings.TrimSpace(get("COMPOSER", "composer")),
		TrackNum:    parsePosInt(get("TRACK", "track", "TRACKNUMBER", "tracknumber")),
		Disc:        parsePosInt(get("DISC", "disc", "DISCNUMBER", "discnumber")),
		Size:        fi.Size(),
	}

	// Fallbacks
	if t.Album == "" {
		t.Album = "Unknown Album"
	}
	if t.Artist == "" && t.AlbumArtist != "" {
		t.Artist = t.AlbumArtist
	}
	if t.Artist == "" {
		t.Artist = "Unknown Artist"
	}
	if t.AlbumArtist == "" {
		t.AlbumArtist = t.Artist
	}
	if t.Disc <= 0 {
		t.Disc = 1
	}
	if t.TrackNum <= 0 {
		t.TrackNum = inferLeadingTrackNumber(filepath.Base(path))
	}

	t.Bucket = bucketFor(t.AlbumArtist)
	t.AlbumKey = albumKey(t.AlbumArtist, t.Album)

	return t, nil
}

// buildLibrary constructs the hierarchical tree index from a flat track list.
func buildLibrary(tracks []*Track) *Library {
	lib := &Library{
		Tracks: tracks,
		Tree:   make(map[string]map[string]map[string]map[string][]*Track),
	}

	// Determine max disc per album to decide whether disc folders are needed.
	albumMaxDisc := make(map[string]int)
	for _, t := range tracks {
		lib.TotalSize += t.Size
		if t.Disc > albumMaxDisc[t.AlbumKey] {
			albumMaxDisc[t.AlbumKey] = t.Disc
		}
	}

	for _, t := range tracks {
		bucket := t.Bucket
		artist := t.AlbumArtist
		album := t.Album
		disc := ""
		if albumMaxDisc[t.AlbumKey] > 1 {
			disc = fmt.Sprintf("Disc %02d", t.Disc)
		}

		if lib.Tree[bucket] == nil {
			lib.Tree[bucket] = make(map[string]map[string]map[string][]*Track)
		}
		if lib.Tree[bucket][artist] == nil {
			lib.Tree[bucket][artist] = make(map[string]map[string][]*Track)
		}
		if lib.Tree[bucket][artist][album] == nil {
			lib.Tree[bucket][artist][album] = make(map[string][]*Track)
		}
		lib.Tree[bucket][artist][album][disc] = append(lib.Tree[bucket][artist][album][disc], t)
	}

	// Sort tracks within each disc group by track number.
	for _, artists := range lib.Tree {
		for _, albums := range artists {
			for _, discs := range albums {
				for _, tks := range discs {
					sort.Slice(tks, func(i, j int) bool {
						if tks[i].TrackNum != tks[j].TrackNum {
							return tks[i].TrackNum < tks[j].TrackNum
						}
						return tks[i].SrcPath < tks[j].SrcPath
					})
				}
			}
		}
	}

	return lib
}

// ---------------------------------------------------------------------------
// Copying
// ---------------------------------------------------------------------------

type CopyProgress struct {
	CurrentFile string
	FilesDone   int
	FilesTotal  int
	Skipped     int
	Failed      int
	BytesDone   int64
	BytesTotal  int64
}

// CopyTracks copies selected tracks to dstRoot with the standard library layout.
func CopyTracks(ctx context.Context, tracks []*Track, dstRoot string, workers int, progressFn func(CopyProgress)) (okCount, skipCount, failCount int, err error) {
	if workers < 1 {
		workers = 1
	}

	// Compute destination paths.
	albumMaxDisc := make(map[string]int)
	for _, t := range tracks {
		if t.Disc > albumMaxDisc[t.AlbumKey] {
			albumMaxDisc[t.AlbumKey] = t.Disc
		}
	}
	var totalBytes int64
	for _, t := range tracks {
		totalBytes += t.Size
		albumDir := filepath.Join(dstRoot, t.Bucket, sanitizePathElem(t.AlbumArtist), sanitizePathElem(t.Album))
		dstDir := albumDir
		if albumMaxDisc[t.AlbumKey] > 1 {
			dstDir = filepath.Join(albumDir, fmt.Sprintf("Disc %02d", t.Disc))
		}
		t.DstDir = dstDir

		title := t.Title
		if title == "" {
			base := filepath.Base(t.SrcPath)
			title = strings.TrimSuffix(base, filepath.Ext(base))
		}
		ext := strings.ToLower(filepath.Ext(t.SrcPath))
		if t.TrackNum > 0 {
			t.DstPath = filepath.Join(dstDir, fmt.Sprintf("%03d - %s%s", t.TrackNum, sanitizePathElem(title), ext))
		} else {
			t.DstPath = filepath.Join(dstDir, fmt.Sprintf("000 - %s%s", sanitizePathElem(title), ext))
		}
	}

	jobs := make(chan *Track, workers*2)
	var wg sync.WaitGroup
	var ok, skip, fail uint64
	var bytesDone int64
	var lastFile atomic.Value
	lastFile.Store("")

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if ctx.Err() != nil {
					return
				}
				lastFile.Store(filepath.Base(t.SrcPath))
				// Skip if destination exists with same size.
				if fi, serr := os.Stat(t.DstPath); serr == nil && fi.Size() == t.Size && t.Size > 0 {
					atomic.AddUint64(&skip, 1)
					atomic.AddInt64(&bytesDone, t.Size)
					continue
				}
				if cerr := copyFile(ctx, t); cerr != nil {
					if ctx.Err() != nil {
						return // cancelled — not a real failure
					}
					atomic.AddUint64(&fail, 1)
					continue
				}
				atomic.AddUint64(&ok, 1)
				atomic.AddInt64(&bytesDone, t.Size)
			}
		}()
	}

	// Progress ticker: single goroutine updates UI at a safe interval.
	done := make(chan struct{})
	if progressFn != nil {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					progressFn(CopyProgress{
						CurrentFile: lastFile.Load().(string),
						FilesDone:   int(atomic.LoadUint64(&ok) + atomic.LoadUint64(&skip) + atomic.LoadUint64(&fail)),
						FilesTotal:  len(tracks),
						Skipped:     int(atomic.LoadUint64(&skip)),
						Failed:      int(atomic.LoadUint64(&fail)),
						BytesDone:   atomic.LoadInt64(&bytesDone),
						BytesTotal:  totalBytes,
					})
				case <-done:
					// Final update.
					progressFn(CopyProgress{
						CurrentFile: lastFile.Load().(string),
						FilesDone:   int(atomic.LoadUint64(&ok) + atomic.LoadUint64(&skip) + atomic.LoadUint64(&fail)),
						FilesTotal:  len(tracks),
						Skipped:     int(atomic.LoadUint64(&skip)),
						Failed:      int(atomic.LoadUint64(&fail)),
						BytesDone:   atomic.LoadInt64(&bytesDone),
						BytesTotal:  totalBytes,
					})
					return
				}
			}
		}()
	}

feed:
	for _, t := range tracks {
		select {
		case jobs <- t:
		case <-ctx.Done():
			break feed // labelled: a bare break would only exit the select
		}
	}
	close(jobs)
	wg.Wait()
	close(done)

	return int(ok), int(skip), int(fail), ctx.Err()
}

// copyChunkSize is the buffer used per read/write during a copy. A large buffer
// minimises SMB round-trips when reading from the NAS over the 2.5 Gbps link and
// keeps writes to the single-channel micro-SD card large and sequential.
const copyChunkSize = 4 << 20 // 4 MiB

var copyBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, copyChunkSize)
		return &b
	},
}

func copyFile(ctx context.Context, t *Track) error {
	if err := os.MkdirAll(t.DstDir, 0o755); err != nil {
		return err
	}
	in, err := os.Open(t.SrcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(t.DstPath)
	if err != nil {
		return err
	}

	bufp := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(bufp)
	buf := *bufp

	for {
		// Honour cancellation between chunks so a Cancel click is observed
		// within one buffer's worth of I/O, not after the whole file.
		if cerr := ctx.Err(); cerr != nil {
			out.Close()
			os.Remove(t.DstPath) // discard the partial file
			return cerr
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}

	// No per-file fsync. Flushing every file to flash forces the card's
	// controller to commit and garbage-collect constantly, which dominates
	// runtime on micro-SD. Close hands the data to the OS; ejecting the card
	// ("Safely Remove Hardware") guarantees the final flush to the device.
	return out.Close()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func bucketFor(albumArtist string) string {
	s := strings.TrimSpace(albumArtist)
	if s == "" {
		return "_"
	}
	r := []rune(strings.ToUpper(s))[0]
	if r >= 'A' && r <= 'Z' {
		return string(r)
	}
	if r >= '0' && r <= '9' {
		return "0-9"
	}
	return "_"
}

func albumKey(albumArtist, album string) string {
	return strings.ToLower(strings.TrimSpace(albumArtist)) + "|" + strings.ToLower(strings.TrimSpace(album))
}

func sanitizePathElem(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	repl := strings.NewReplacer(
		"/", "\uff0f", "\\", "\uff3c", ":", "\uff1a", "*", "\uff0a",
		"?", "\uff1f", "\"", "\uff02", "<", "\uff1c", ">", "\uff1e", "|", "\uff5c",
	)
	s = repl.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func parsePosInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func inferLeadingTrackNumber(name string) int {
	m := reLeadingNum.FindStringSubmatch(name)
	if len(m) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// TrackDisplayName returns a human-readable track name for display.
func TrackDisplayName(t *Track) string {
	title := t.Title
	if title == "" {
		base := filepath.Base(t.SrcPath)
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if t.TrackNum > 0 {
		return fmt.Sprintf("%03d - %s", t.TrackNum, title)
	}
	return title
}

// FormatSize returns a human-readable size string.
func FormatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1024*1024*1024))
	case bytes >= 1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%.1f KiB", float64(bytes)/1024)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// bytesPerSec returns the average rate in bytes/second over the elapsed time.
func bytesPerSec(bytes int64, d time.Duration) float64 {
	s := d.Seconds()
	if s <= 0 {
		return 0
	}
	return float64(bytes) / s
}

// FormatRate returns a human-readable transfer rate (e.g. "18.4 MiB/s").
func FormatRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "0 B/s"
	}
	return FormatSize(int64(bytesPerSecond)) + "/s"
}

// FormatDuration returns a compact M:SS or H:MM:SS duration string.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
