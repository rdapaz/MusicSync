package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Tree node UID scheme
//
// UIDs are pipe-delimited strings whose depth encodes the level:
//   depth 1: "A"                         → bucket
//   depth 2: "A|ABBA"                    → artist
//   depth 3: "A|ABBA|Gold"               → album
//   depth 4: "A|ABBA|Gold|Disc 01"       → disc  (only when multi-disc)
//   depth 4: "A|ABBA|Gold|#3"            → track (single-disc album, index into slice)
//   depth 5: "A|ABBA|Gold|Disc 01|#3"    → track (multi-disc album)
//
// Track indices are prefixed with "#" to distinguish them from disc labels.
// ---------------------------------------------------------------------------

const uidSep = "|"

func uidParts(uid string) []string {
	if uid == "" {
		return nil
	}
	return strings.Split(uid, uidSep)
}

func uidJoin(parts ...string) string {
	return strings.Join(parts, uidSep)
}

func isTrackUID(uid string) bool {
	parts := uidParts(uid)
	if len(parts) < 4 {
		return false
	}
	last := parts[len(parts)-1]
	return strings.HasPrefix(last, "#")
}

func trackIndex(uid string) int {
	parts := uidParts(uid)
	last := parts[len(parts)-1]
	n, _ := strconv.Atoi(strings.TrimPrefix(last, "#"))
	return n
}

// ---------------------------------------------------------------------------
// Precomputed node info (built once, used on every render)
// ---------------------------------------------------------------------------

type nodeCache struct {
	tracks    []*Track
	totalSize int64
	label     string // pre-formatted display label
}

// ---------------------------------------------------------------------------
// TreeModel
// ---------------------------------------------------------------------------

type TreeModel struct {
	lib       *Library
	selected  map[*Track]bool
	budgetMax int64
	onChange  func()

	// Precomputed caches keyed by UID.
	cache    map[string]*nodeCache
	children map[string][]string // parent UID → sorted child UIDs
}

func NewTreeModel(lib *Library, budgetMax int64, onChange func()) *TreeModel {
	m := &TreeModel{
		lib:       lib,
		selected:  make(map[*Track]bool),
		budgetMax: budgetMax,
		onChange:  onChange,
		cache:     make(map[string]*nodeCache),
		children:  make(map[string][]string),
	}
	m.buildCache()
	return m
}

// buildCache precomputes all node data so rendering is instant.
func (m *TreeModel) buildCache() {
	if m.lib == nil {
		return
	}

	// Root children: buckets
	bucketKeys := sortedKeys(m.lib.Tree)
	rootChildren := make([]string, len(bucketKeys))
	for i, k := range bucketKeys {
		rootChildren[i] = k
	}
	m.children[""] = rootChildren

	for _, bucket := range bucketKeys {
		artists := m.lib.Tree[bucket]
		bucketUID := bucket

		// Bucket → artists
		artistKeys := sortedMapKeys(artists)
		artistUIDs := make([]string, len(artistKeys))
		for i, k := range artistKeys {
			artistUIDs[i] = uidJoin(bucket, k)
		}
		m.children[bucketUID] = artistUIDs

		var bucketTracks []*Track
		var bucketSize int64

		for _, artistName := range artistKeys {
			albums := artists[artistName]
			artistUID := uidJoin(bucket, artistName)

			// Artist → albums
			albumKeys := sortedMapKeys2(albums)
			albumUIDs := make([]string, len(albumKeys))
			for i, k := range albumKeys {
				albumUIDs[i] = uidJoin(bucket, artistName, k)
			}
			m.children[artistUID] = albumUIDs

			var artistTracks []*Track
			var artistSize int64

			for _, albumName := range albumKeys {
				discs := albums[albumName]
				albumUID := uidJoin(bucket, artistName, albumName)

				var albumTracks []*Track
				var albumSize int64

				// Determine if single-disc or multi-disc
				isSingleDisc := len(discs) == 1 && func() bool { _, ok := discs[""]; return ok }()

				if isSingleDisc {
					tracks := discs[""]
					// Album → tracks directly
					trackUIDs := make([]string, len(tracks))
					for i, t := range tracks {
						tUID := uidJoin(albumUID, fmt.Sprintf("#%d", i))
						trackUIDs[i] = tUID
						m.cache[tUID] = &nodeCache{
							tracks:    []*Track{t},
							totalSize: t.Size,
							label:     fmt.Sprintf("%s  (%s)", TrackDisplayName(t), FormatSize(t.Size)),
						}
					}
					m.children[albumUID] = trackUIDs
					albumTracks = tracks
					for _, t := range tracks {
						albumSize += t.Size
					}
				} else {
					// Multi-disc
					discKeys := sortedMapKeys3(discs)
					discUIDs := make([]string, len(discKeys))
					for i, k := range discKeys {
						discUIDs[i] = uidJoin(bucket, artistName, albumName, k)
					}
					m.children[albumUID] = discUIDs

					for _, discName := range discKeys {
						tracks := discs[discName]
						discUID := uidJoin(bucket, artistName, albumName, discName)

						// Disc → tracks
						trackUIDs := make([]string, len(tracks))
						var discSize int64
						for i, t := range tracks {
							tUID := uidJoin(discUID, fmt.Sprintf("#%d", i))
							trackUIDs[i] = tUID
							discSize += t.Size
							m.cache[tUID] = &nodeCache{
								tracks:    []*Track{t},
								totalSize: t.Size,
								label:     fmt.Sprintf("%s  (%s)", TrackDisplayName(t), FormatSize(t.Size)),
							}
						}
						m.children[discUID] = trackUIDs

						m.cache[discUID] = &nodeCache{
							tracks:    tracks,
							totalSize: discSize,
							label:     fmt.Sprintf("%s  (%d tracks)", discName, len(tracks)),
						}
						albumTracks = append(albumTracks, tracks...)
						albumSize += discSize
					}
				}

				m.cache[albumUID] = &nodeCache{
					tracks:    albumTracks,
					totalSize: albumSize,
					label:     fmt.Sprintf("%s  (%d tracks, %s)", albumName, len(albumTracks), FormatSize(albumSize)),
				}
				artistTracks = append(artistTracks, albumTracks...)
				artistSize += albumSize
			}

			m.cache[artistUID] = &nodeCache{
				tracks:    artistTracks,
				totalSize: artistSize,
				label:     fmt.Sprintf("%s  (%d tracks, %s)", artistName, len(artistTracks), FormatSize(artistSize)),
			}
			bucketTracks = append(bucketTracks, artistTracks...)
			bucketSize += artistSize
		}

		m.cache[bucketUID] = &nodeCache{
			tracks:    bucketTracks,
			totalSize: bucketSize,
			label:     fmt.Sprintf("%s  (%d tracks)", bucket, len(bucketTracks)),
		}
	}
}

// ChildUIDs returns precomputed child node IDs.
func (m *TreeModel) ChildUIDs(uid string) []string {
	return m.children[uid]
}

// IsBranch returns true if the UID is not a track.
func (m *TreeModel) IsBranch(uid string) bool {
	return !isTrackUID(uid)
}

// TracksForUID returns the cached track list for a node.
func (m *TreeModel) TracksForUID(uid string) []*Track {
	if c, ok := m.cache[uid]; ok {
		return c.tracks
	}
	return nil
}

// NodeLabel returns the precomputed display text for a node.
func (m *TreeModel) NodeLabel(uid string) string {
	if c, ok := m.cache[uid]; ok {
		return c.label
	}
	return uid
}

func (m *TreeModel) trackByUID(uid string) *Track {
	if c, ok := m.cache[uid]; ok && len(c.tracks) == 1 {
		return c.tracks[0]
	}
	return nil
}

// ---------------------------------------------------------------------------
// Selection state
// ---------------------------------------------------------------------------

// IsChecked returns the check state for a node.
func (m *TreeModel) IsChecked(uid string) (checked, partial bool) {
	c, ok := m.cache[uid]
	if !ok || len(c.tracks) == 0 {
		return false, false
	}

	var have, notHave int
	for _, t := range c.tracks {
		if m.selected[t] {
			have++
		} else {
			notHave++
		}
		if have > 0 && notHave > 0 {
			return false, true
		}
	}

	if have == len(c.tracks) {
		return true, false
	}
	return false, false
}

// Toggle toggles the selection of all tracks under uid.
func (m *TreeModel) Toggle(uid string) {
	c, ok := m.cache[uid]
	if !ok || len(c.tracks) == 0 {
		return
	}

	checked, partial := m.IsChecked(uid)
	wantSelect := !checked || partial

	if wantSelect {
		var needed int64
		for _, t := range c.tracks {
			if !m.selected[t] {
				needed += t.Size
			}
		}
		if m.budgetMax > 0 && m.SelectedBytes()+needed > m.budgetMax {
			remaining := m.budgetMax - m.SelectedBytes()
			for _, t := range c.tracks {
				if !m.selected[t] && t.Size <= remaining {
					m.selected[t] = true
					remaining -= t.Size
				}
			}
			if m.onChange != nil {
				m.onChange()
			}
			return
		}
		for _, t := range c.tracks {
			m.selected[t] = true
		}
	} else {
		for _, t := range c.tracks {
			delete(m.selected, t)
		}
	}

	if m.onChange != nil {
		m.onChange()
	}
}

// SelectAll selects all tracks if they fit within budget.
func (m *TreeModel) SelectAll() {
	if m.lib == nil {
		return
	}
	var total int64
	for _, t := range m.lib.Tracks {
		total += t.Size
	}
	if m.budgetMax > 0 && total > m.budgetMax {
		var used int64
		for _, t := range m.lib.Tracks {
			if used+t.Size <= m.budgetMax {
				m.selected[t] = true
				used += t.Size
			}
		}
	} else {
		for _, t := range m.lib.Tracks {
			m.selected[t] = true
		}
	}
	if m.onChange != nil {
		m.onChange()
	}
}

// DeselectAll clears all selection.
func (m *TreeModel) DeselectAll() {
	m.selected = make(map[*Track]bool)
	if m.onChange != nil {
		m.onChange()
	}
}

// SelectedBytes returns the total size of selected tracks.
func (m *TreeModel) SelectedBytes() int64 {
	var total int64
	for t := range m.selected {
		total += t.Size
	}
	return total
}

// SelectedTracks returns a slice of all selected tracks.
func (m *TreeModel) SelectedTracks() []*Track {
	out := make([]*Track, 0, len(m.selected))
	for t := range m.selected {
		out = append(out, t)
	}
	return out
}

// SelectedPaths returns the source paths of all selected tracks, for persistence.
func (m *TreeModel) SelectedPaths() []string {
	out := make([]string, 0, len(m.selected))
	for t := range m.selected {
		out = append(out, t.SrcPath)
	}
	return out
}

// RestoreSelection marks every track whose source path is in the given set as
// selected. Paths no longer present in the library are ignored. Budget limits
// are intentionally not enforced here — we restore exactly what was saved and
// let the budget bar reflect any overage. Returns the number of tracks selected.
func (m *TreeModel) RestoreSelection(paths map[string]bool) int {
	if m.lib == nil || len(paths) == 0 {
		return 0
	}
	n := 0
	for _, t := range m.lib.Tracks {
		if paths[t.SrcPath] {
			m.selected[t] = true
			n++
		}
	}
	return n
}

// SelectedTrackCount returns the number of selected tracks.
func (m *TreeModel) SelectedTrackCount() int {
	return len(m.selected)
}

// SelectedAlbumCount returns the number of distinct albums with selected tracks.
func (m *TreeModel) SelectedAlbumCount() int {
	seen := make(map[string]struct{})
	for t := range m.selected {
		seen[t.AlbumKey] = struct{}{}
	}
	return len(seen)
}

// SetBudget updates the budget limit.
func (m *TreeModel) SetBudget(bytes int64) {
	m.budgetMax = bytes
}

// ---------------------------------------------------------------------------
// Sorted key helpers
// ---------------------------------------------------------------------------

func sortedKeys(m map[string]map[string]map[string]map[string][]*Track) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys(m map[string]map[string]map[string][]*Track) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys2(m map[string]map[string][]*Track) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys3(m map[string][]*Track) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
