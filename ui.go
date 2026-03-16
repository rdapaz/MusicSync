package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"image/color"
)

// buildUI constructs the entire application UI and returns the root canvas object.
func buildUI(a fyne.App, w fyne.Window) fyne.CanvasObject {
	prefs := a.Preferences()

	// -----------------------------------------------------------------------
	// State — restore from preferences
	// -----------------------------------------------------------------------
	var lib *Library
	var model *TreeModel
	var cancelFn context.CancelFunc

	savedSrcDirs := prefs.StringWithFallback("source_dirs", "")
	sourceDirs := []string{}
	if savedSrcDirs != "" {
		for _, d := range strings.Split(savedSrcDirs, "\n") {
			d = strings.TrimSpace(d)
			if d != "" {
				sourceDirs = append(sourceDirs, d)
			}
		}
	}

	savePrefs := func() {
		prefs.SetString("source_dirs", strings.Join(sourceDirs, "\n"))
	}

	// -----------------------------------------------------------------------
	// Source directory list
	// -----------------------------------------------------------------------
	var sourceList *widget.List
	sourceList = widget.NewList(
		func() int { return len(sourceDirs) },
		func() fyne.CanvasObject {
			return container.NewHBox(
				widget.NewLabel(""),
				layout.NewSpacer(),
				widget.NewButtonWithIcon("", theme.DeleteIcon(), nil),
			)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			box := obj.(*fyne.Container)
			label := box.Objects[0].(*widget.Label)
			removeBtn := box.Objects[2].(*widget.Button)
			label.SetText(sourceDirs[id])
			removeBtn.OnTapped = func() {
				sourceDirs = append(sourceDirs[:id], sourceDirs[id+1:]...)
				sourceList.Refresh()
				savePrefs()
			}
		},
	)

	addSourceBtn := widget.NewButtonWithIcon("Add Source", theme.FolderNewIcon(), func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if uri != nil {
				sourceDirs = append(sourceDirs, uri.Path())
				sourceList.Refresh()
				savePrefs()
			}
		}, w)
	})

	// -----------------------------------------------------------------------
	// Destination
	// -----------------------------------------------------------------------
	dstEntry := widget.NewEntry()
	dstEntry.SetPlaceHolder("Destination directory")
	dstEntry.SetText(prefs.StringWithFallback("destination", ""))
	dstEntry.OnChanged = func(s string) {
		prefs.SetString("destination", s)
	}
	dstBrowse := widget.NewButtonWithIcon("Browse", theme.FolderOpenIcon(), func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if uri != nil {
				dstEntry.SetText(uri.Path())
			}
		}, w)
	})

	// -----------------------------------------------------------------------
	// Budget (SD card sizes), Reserve, Workers
	// -----------------------------------------------------------------------
	sdCardSizes := map[string]float64{
		"32 GB":  32,
		"64 GB":  64,
		"128 GB": 128,
		"256 GB": 256,
		"512 GB": 512,
		"1 TB":   1000,
		"2 TB":   2000,
	}
	sdCardOptions := []string{"32 GB", "64 GB", "128 GB", "256 GB", "512 GB", "1 TB", "2 TB"}
	savedBudget := prefs.StringWithFallback("budget", "64 GB")
	budgetGB := 64.0
	if gb, ok := sdCardSizes[savedBudget]; ok {
		budgetGB = gb
	}

	budgetSelect := widget.NewSelect(sdCardOptions, nil)
	budgetSelect.SetSelected(savedBudget)

	reserveOptions := []string{"0%", "5%", "8%", "10%", "15%", "20%"}
	reservePctMap := map[string]float64{"0%": 0, "5%": 5, "8%": 8, "10%": 10, "15%": 15, "20%": 20}
	savedReserve := prefs.StringWithFallback("reserve", "8%")
	reservePct := 8.0
	if pct, ok := reservePctMap[savedReserve]; ok {
		reservePct = pct
	}

	reserveSelect := widget.NewSelect(reserveOptions, nil)
	reserveSelect.SetSelected(savedReserve)

	numCPU := runtime.NumCPU()
	workerOptions := make([]string, 0, numCPU)
	for i := 1; i <= numCPU; i++ {
		workerOptions = append(workerOptions, fmt.Sprintf("%d", i))
	}
	workerCount := numCPU

	workerSelect := widget.NewSelect(workerOptions, nil)
	workerSelect.SetSelected(fmt.Sprintf("%d", numCPU))

	usableBytes := func() int64 {
		nominal := int64(budgetGB * 1_000_000_000)
		return int64(float64(nominal) * (1.0 - reservePct/100.0))
	}

	budgetSelect.OnChanged = func(s string) {
		if gb, ok := sdCardSizes[s]; ok {
			budgetGB = gb
		}
		prefs.SetString("budget", s)
		if model != nil {
			model.SetBudget(usableBytes())
		}
	}
	reserveSelect.OnChanged = func(s string) {
		if pct, ok := reservePctMap[s]; ok {
			reservePct = pct
		}
		prefs.SetString("reserve", s)
		if model != nil {
			model.SetBudget(usableBytes())
		}
	}
	workerSelect.OnChanged = func(s string) {
		n, _ := fmt.Sscan(s, &workerCount)
		_ = n
	}

	// -----------------------------------------------------------------------
	// Progress bar (reused for scan, probe, copy)
	// -----------------------------------------------------------------------
	progressBar := widget.NewProgressBar()
	progressBar.Hide()
	progressLabel := widget.NewLabel("")
	progressLabel.Hide()

	// -----------------------------------------------------------------------
	// Detail panel (right side)
	// -----------------------------------------------------------------------
	detailTitle := canvas.NewText("MusicSync", color.NRGBA{R: 130, G: 180, B: 255, A: 255})
	detailTitle.TextSize = 22
	detailTitle.TextStyle = fyne.TextStyle{Bold: true}

	detailSubtitle := widget.NewLabel("Add source folders and scan your library to begin.")
	detailSubtitle.Wrapping = fyne.TextWrapWord

	detailContent := widget.NewRichText()
	detailContent.Wrapping = fyne.TextWrapWord

	detailPanel := container.NewVBox(
		detailTitle,
		widget.NewSeparator(),
		detailSubtitle,
		detailContent,
	)
	detailScroll := container.NewScroll(detailPanel)

	updateDetail := func(uid string) {
		if model == nil || uid == "" {
			return
		}
		tracks := model.TracksForUID(uid)
		if len(tracks) == 0 {
			return
		}

		parts := uidParts(uid)

		if isTrackUID(uid) {
			t := tracks[0]
			detailTitle.Text = TrackDisplayName(t)
			detailTitle.Refresh()
			detailSubtitle.SetText(fmt.Sprintf("%s  -  %s", t.AlbumArtist, t.Album))

			lines := []widget.RichTextSegment{
				&widget.TextSegment{Text: fmt.Sprintf("Artist:        %s", t.Artist), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Album Artist:  %s", t.AlbumArtist), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Album:         %s", t.Album), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Genre:         %s", t.Genre), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Year:          %s", t.Date), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Composer:      %s", t.Composer), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Track:         %d", t.TrackNum), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Disc:          %d", t.Disc), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Size:          %s", FormatSize(t.Size)), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Format:        %s", strings.ToUpper(strings.TrimPrefix(strings.ToLower(t.SrcPath[strings.LastIndex(t.SrcPath, "."):]), "."))), Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: "", Style: widget.RichTextStyleCodeInline},
				&widget.TextSegment{Text: fmt.Sprintf("Source: %s", t.SrcPath), Style: widget.RichTextStyleCodeInline},
			}
			detailContent.Segments = lines
			detailContent.Refresh()
			return
		}

		// Branch node: show summary + track listing.
		var totalSize int64
		for _, t := range tracks {
			totalSize += t.Size
		}

		// Title based on depth.
		switch len(parts) {
		case 1:
			detailTitle.Text = fmt.Sprintf("Bucket: %s", parts[0])
		case 2:
			detailTitle.Text = parts[1] // artist name
		case 3:
			detailTitle.Text = parts[2] // album name
		case 4:
			detailTitle.Text = parts[3] // disc
		}
		detailTitle.Refresh()

		// Gather distinct values for summary.
		artists := distinctField(tracks, func(t *Track) string { return t.AlbumArtist })
		albums := distinctField(tracks, func(t *Track) string { return t.Album })
		genres := distinctField(tracks, func(t *Track) string { return t.Genre })
		dates := distinctField(tracks, func(t *Track) string { return t.Date })

		summaryParts := []string{
			fmt.Sprintf("Tracks: %d", len(tracks)),
			fmt.Sprintf("Size:   %s", FormatSize(totalSize)),
		}
		if len(parts) <= 2 {
			summaryParts = append(summaryParts, fmt.Sprintf("Albums: %d", len(albums)))
		}
		if len(parts) == 1 {
			summaryParts = append(summaryParts, fmt.Sprintf("Artists: %d", len(artists)))
		}
		if len(genres) > 0 && len(genres) <= 5 {
			summaryParts = append(summaryParts, fmt.Sprintf("Genre:  %s", strings.Join(genres, ", ")))
		}
		if len(dates) > 0 && len(dates) <= 3 {
			summaryParts = append(summaryParts, fmt.Sprintf("Year:   %s", strings.Join(dates, ", ")))
		}

		detailSubtitle.SetText(strings.Join(summaryParts, "\n"))

		// Track listing (for albums and discs).
		if len(parts) >= 3 && len(tracks) <= 200 {
			lines := make([]widget.RichTextSegment, 0, len(tracks)+1)
			lines = append(lines, &widget.TextSegment{Text: "Track Listing:", Style: widget.RichTextStyleSubHeading})
			for _, t := range tracks {
				line := fmt.Sprintf("  %s    %s", TrackDisplayName(t), FormatSize(t.Size))
				lines = append(lines, &widget.TextSegment{Text: line, Style: widget.RichTextStyleCodeInline})
			}
			detailContent.Segments = lines
		} else {
			detailContent.Segments = nil
		}
		detailContent.Refresh()
	}

	// -----------------------------------------------------------------------
	// Library tree (left side)
	// -----------------------------------------------------------------------
	var tree *widget.Tree

	// -----------------------------------------------------------------------
	// Budget status bar
	// -----------------------------------------------------------------------
	budgetBar := widget.NewProgressBar()
	budgetBar.Min = 0
	budgetBar.Max = 1

	budgetText := canvas.NewText("No library loaded", color.NRGBA{R: 180, G: 180, B: 180, A: 255})
	budgetText.TextSize = 13
	budgetText.TextStyle = fyne.TextStyle{Bold: true}

	updateBudget := func() {
		if model == nil {
			return
		}
		sel := model.SelectedBytes()
		ub := usableBytes()
		pct := float64(0)
		if ub > 0 {
			pct = float64(sel) / float64(ub)
		}
		budgetBar.SetValue(pct)

		text := fmt.Sprintf("Selected: %s / %s usable  |  %d tracks  |  %d albums  |  %.0f%%",
			FormatSize(sel), FormatSize(ub),
			model.SelectedTrackCount(), model.SelectedAlbumCount(),
			pct*100)
		budgetText.Text = text

		if pct > 0.95 {
			budgetText.Color = color.NRGBA{R: 255, G: 80, B: 80, A: 255}
		} else if pct > 0.75 {
			budgetText.Color = color.NRGBA{R: 255, G: 200, B: 80, A: 255}
		} else {
			budgetText.Color = color.NRGBA{R: 100, G: 220, B: 130, A: 255}
		}
		budgetText.Refresh()
	}

	// -----------------------------------------------------------------------
	// Action buttons
	// -----------------------------------------------------------------------
	selectAllBtn := widget.NewButtonWithIcon("Select All", theme.CheckButtonCheckedIcon(), func() {
		if model != nil {
			model.SelectAll()
			if tree != nil {
				tree.Refresh()
			}
			updateBudget()
		}
	})
	selectAllBtn.Disable()

	deselectAllBtn := widget.NewButtonWithIcon("Deselect All", theme.CheckButtonIcon(), func() {
		if model != nil {
			model.DeselectAll()
			if tree != nil {
				tree.Refresh()
			}
			updateBudget()
		}
	})
	deselectAllBtn.Disable()

	copyBtn := widget.NewButtonWithIcon("Copy Selected", theme.MailSendIcon(), nil)
	copyBtn.Importance = widget.HighImportance
	copyBtn.Disable()

	cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
		if cancelFn != nil {
			cancelFn()
		}
	})
	cancelBtn.Disable()

	// -----------------------------------------------------------------------
	// Tree widget creation
	// -----------------------------------------------------------------------
	tree = widget.NewTree(
		func(uid widget.TreeNodeID) []widget.TreeNodeID { // childUIDs
			if model == nil {
				return nil
			}
			return model.ChildUIDs(uid)
		},
		func(uid widget.TreeNodeID) bool { // isBranch
			if model == nil {
				return false
			}
			return model.IsBranch(uid)
		},
		func(branch bool) fyne.CanvasObject { // createNode
			check := widget.NewCheck("", nil)
			label := widget.NewLabel("placeholder text")
			return container.NewBorder(nil, nil, check, nil, label)
		},
		func(uid widget.TreeNodeID, branch bool, node fyne.CanvasObject) { // updateNode
			if model == nil {
				return
			}
			box := node.(*fyne.Container)
			// Border layout stores objects as: [center, left, ...] so find by type.
			var check *widget.Check
			var label *widget.Label
			for _, obj := range box.Objects {
				switch v := obj.(type) {
				case *widget.Check:
					check = v
				case *widget.Label:
					label = v
				}
			}
			if check == nil || label == nil {
				return
			}

			checked, partial := model.IsChecked(uid)

			// Fyne Check is binary; for partial state, show checked with a visual hint.
			check.Checked = checked || partial
			check.OnChanged = func(b bool) {
				model.Toggle(uid)
				tree.Refresh()
				updateBudget()
			}
			check.Refresh()

			text := model.NodeLabel(uid)
			if partial {
				text = "~ " + text
			}
			label.SetText(text)
		},
	)

	tree.OnSelected = func(uid widget.TreeNodeID) {
		updateDetail(uid)
	}

	// -----------------------------------------------------------------------
	// Scan button
	// -----------------------------------------------------------------------
	scanBtn := widget.NewButtonWithIcon("Scan Library", theme.SearchIcon(), nil)
	scanBtn.Importance = widget.HighImportance

	// Helper: called after library is loaded (from cache or fresh scan).
	applyLibrary := func(newLib *Library, fromCache bool) {
		lib = newLib
		model = NewTreeModel(lib, usableBytes(), func() {
			updateBudget()
		})

		tree.Refresh()
		progressBar.Hide()
		source := "scanned"
		if fromCache {
			source = "loaded from cache"
		}
		progressLabel.SetText(fmt.Sprintf("Library: %d tracks, %s total (%s)",
			len(lib.Tracks), FormatSize(lib.TotalSize), source))
		progressLabel.Show()

		detailTitle.Text = "Library Ready"
		detailTitle.Refresh()
		detailSubtitle.SetText(fmt.Sprintf(
			"%d tracks found across %d artists.\nSelect albums and tracks, then click Copy.",
			len(lib.Tracks), countArtists(lib)))
		detailContent.Segments = nil
		detailContent.Refresh()

		scanBtn.Enable()
		selectAllBtn.Enable()
		deselectAllBtn.Enable()
		copyBtn.Enable()
		updateBudget()
	}

	// doFullScan runs the scan+probe pipeline in a background goroutine.
	doFullScan := func() {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			dialog.ShowError(fmt.Errorf("ffprobe not found on PATH — please install FFmpeg"), w)
			return
		}

		scanBtn.Disable()
		selectAllBtn.Disable()
		deselectAllBtn.Disable()
		copyBtn.Disable()
		progressBar.Show()
		progressBar.SetValue(0)
		progressLabel.Show()
		progressLabel.SetText("Scanning...")

		ctx, cancel := context.WithCancel(context.Background())
		cancelFn = cancel
		cancelBtn.Enable()

		dirs := make([]string, len(sourceDirs))
		copy(dirs, sourceDirs)

		go func() {
			defer func() {
				cancelBtn.Disable()
				cancelFn = nil
			}()

			paths, err := ScanSources(ctx, dirs, func(found int) {
				progressLabel.SetText(fmt.Sprintf("Scanning... %d files found", found))
			})
			if err != nil {
				progressBar.Hide()
				progressLabel.Hide()
				scanBtn.Enable()
				dialog.ShowError(err, w)
				return
			}
			if len(paths) == 0 {
				progressBar.Hide()
				progressLabel.Hide()
				scanBtn.Enable()
				dialog.ShowInformation("Scan Complete", "No audio files found in the selected directories.", w)
				return
			}

			progressLabel.SetText(fmt.Sprintf("Probing %d files...", len(paths)))
			progressBar.SetValue(0)

			workers := workerCount
			newLib, err := ProbeFiles(ctx, paths, "ffprobe", workers*2, func(done, total int) {
				if total > 0 {
					progressBar.SetValue(float64(done) / float64(total))
				}
				progressLabel.SetText(fmt.Sprintf("Probing... %d / %d", done, total))
			})
			if err != nil {
				progressBar.Hide()
				progressLabel.Hide()
				scanBtn.Enable()
				dialog.ShowError(err, w)
				return
			}

			if saveErr := SaveLibraryCache(dirs, newLib); saveErr != nil {
				fmt.Printf("Warning: failed to save cache: %v\n", saveErr)
			}

			applyLibrary(newLib, false)
		}()
	}

	scanBtn.OnTapped = func() {
		if len(sourceDirs) == 0 {
			dialog.ShowError(fmt.Errorf("add at least one source directory"), w)
			return
		}

		// Try loading from cache first.
		cached, _ := LoadLibraryCache(sourceDirs)
		if cached != nil {
			applyLibrary(cached, true)
			return
		}

		doFullScan()
	}

	rescanBtn := widget.NewButtonWithIcon("Force Rescan", theme.ViewRefreshIcon(), func() {
		if len(sourceDirs) == 0 {
			dialog.ShowError(fmt.Errorf("add at least one source directory"), w)
			return
		}
		doFullScan()
	})

	// -----------------------------------------------------------------------
	// Copy button handler
	// -----------------------------------------------------------------------
	copyBtn.OnTapped = func() {
		if model == nil {
			return
		}
		dst := strings.TrimSpace(dstEntry.Text)
		if dst == "" {
			dialog.ShowError(fmt.Errorf("set a destination directory first"), w)
			return
		}
		tracks := model.SelectedTracks()
		if len(tracks) == 0 {
			dialog.ShowError(fmt.Errorf("no tracks selected"), w)
			return
		}

		msg := fmt.Sprintf("Copy %d tracks (%s) to:\n%s",
			len(tracks), FormatSize(model.SelectedBytes()), dst)

		dialog.ShowConfirm("Start Copy", msg, func(ok bool) {
			if !ok {
				return
			}

			scanBtn.Disable()
			selectAllBtn.Disable()
			deselectAllBtn.Disable()
			copyBtn.Disable()
			progressBar.Show()
			progressBar.SetValue(0)
			progressLabel.Show()
			progressLabel.SetText("Copying...")

			ctx, cancel := context.WithCancel(context.Background())
			cancelFn = cancel
			cancelBtn.Enable()

			go func() {
				defer func() {
					cancelBtn.Disable()
					cancelFn = nil
				}()

				workers := workerCount
				okN, skipN, failN, err := CopyTracks(ctx, tracks, dst, workers, func(p CopyProgress) {
					if p.FilesTotal > 0 {
						progressBar.SetValue(float64(p.FilesDone) / float64(p.FilesTotal))
					}
					progressLabel.SetText(fmt.Sprintf("Copying %d/%d  |  %s  |  %s",
						p.FilesDone, p.FilesTotal, FormatSize(p.BytesDone), p.CurrentFile))
				})

				progressBar.Hide()

				if err != nil && ctx.Err() != nil {
					progressLabel.SetText("Copy cancelled.")
				} else {
					progressLabel.SetText(fmt.Sprintf("Done: %d copied, %d skipped, %d failed", okN, skipN, failN))
					summary := fmt.Sprintf("Copy complete!\n\nCopied: %d\nSkipped: %d\nFailed: %d",
						okN, skipN, failN)
					dialog.ShowInformation("Copy Complete", summary, w)
				}

				scanBtn.Enable()
				selectAllBtn.Enable()
				deselectAllBtn.Enable()
				copyBtn.Enable()
			}()
		}, w)
	}

	// -----------------------------------------------------------------------
	// Layout assembly
	// -----------------------------------------------------------------------

	// Toolbar row 1: sources
	sourceScroll := container.NewScroll(sourceList)
	sourceScroll.SetMinSize(fyne.NewSize(0, 80))
	sourceRow := container.NewBorder(nil, nil, widget.NewLabel("Sources:"), addSourceBtn, sourceScroll)

	// Toolbar row 2: destination
	destRow := container.NewBorder(nil, nil, widget.NewLabel("Destination:"), dstBrowse, dstEntry)

	// Toolbar row 3: budget / reserve / workers
	slidersRow := container.NewHBox(
		widget.NewLabel("Card Size:"), budgetSelect,
		widget.NewSeparator(),
		widget.NewLabel("Reserve:"), reserveSelect,
		widget.NewSeparator(),
		widget.NewLabel("Workers:"), workerSelect,
	)

	// Toolbar row 4: scan + progress
	scanRow := container.NewHBox(scanBtn, rescanBtn, cancelBtn, layout.NewSpacer(), progressLabel)

	toolbar := container.NewVBox(
		sourceRow,
		destRow,
		slidersRow,
		scanRow,
		progressBar,
		widget.NewSeparator(),
	)

	// Center: HSplit — tree left, detail right
	split := container.NewHSplit(tree, detailScroll)
	split.SetOffset(0.4)

	// Bottom: budget bar + action buttons
	budgetRow := container.NewBorder(nil, nil, nil, nil,
		container.NewVBox(
			container.NewBorder(nil, nil, nil, nil, budgetBar),
			container.NewCenter(budgetText),
		),
	)

	buttonRow := container.NewHBox(
		selectAllBtn,
		deselectAllBtn,
		layout.NewSpacer(),
		copyBtn,
	)

	statusBar := container.NewVBox(
		widget.NewSeparator(),
		budgetRow,
		buttonRow,
	)

	return container.NewBorder(toolbar, statusBar, nil, nil, split)
}

// ---------------------------------------------------------------------------
// UI helpers
// ---------------------------------------------------------------------------

func countArtists(lib *Library) int {
	seen := make(map[string]struct{})
	for _, t := range lib.Tracks {
		seen[t.AlbumArtist] = struct{}{}
	}
	return len(seen)
}

func distinctField(tracks []*Track, fn func(*Track) string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, t := range tracks {
		v := fn(t)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
