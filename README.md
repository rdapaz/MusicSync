# MusicSync

A desktop GUI tool for selectively copying music libraries to SD cards and portable drives. Built with [Fyne](https://fyne.io/) and Go.

MusicSync scans your music directories, reads metadata via `ffprobe`, and presents a browsable tree organised by letter bucket, artist, album, and disc. You pick what fits within your target size budget, hit copy, and get a cleanly structured library on the destination.

## Features

- **Browsable library tree** with tri-state checkboxes (album/artist/bucket level)
- **Size budget enforcement** — choose your SD card size (32 GB to 2 TB) and reserve percentage; selections are capped to what fits
- **Detail panel** showing metadata for the selected artist, album, or track
- **Concurrent scanning and copying** with configurable worker count
- **SQLite cache** — scanned library is persisted locally so subsequent runs load instantly
- **Persistent settings** — source directories, destination, budget, and reserve are remembered between sessions
- **Cross-platform** — builds for Windows, macOS (Intel + Apple Silicon), and Linux

## Screenshot

```
+================================================================+
|  [+ Sources] [Set Destination] | Budget [128 GB] Reserve [8%]  |
+========================+=======================================+
|   LIBRARY TREE         |   DETAIL PANEL                        |
|   [x] A               |   Artist:  ABBA                       |
|     [x] ABBA          |   Album:   Gold (Greatest Hits)        |
|       [x] Gold        |   Tracks:  19  |  Size: 842.3 MB       |
|       [ ] Arrival      |                                       |
|     [ ] AC/DC          |   001 - Dancing Queen        34.2 MB  |
|   [-] B               |   002 - Knowing Me           31.8 MB  |
+========================+=======================================+
|  Selected: 12.4 / 14.0 GiB | 234 tracks | 18 albums           |
|                                  [Copy Selected]  [Cancel]     |
+================================================================+
```

## Prerequisites

- **Go 1.20+**
- **ffprobe** (part of [FFmpeg](https://ffmpeg.org/)) must be on your `PATH`

### Linux only

Fyne requires OpenGL and X11 development headers:

```bash
# Debian / Ubuntu
sudo apt-get install libgl1-mesa-dev xorg-dev
```

## Building from source

```bash
git clone https://github.com/rdapaz/MusicSync.git
cd MusicSync
go build -trimpath -ldflags="-s -w" -o musicsync .
```

## Usage

1. Launch the app and add one or more source directories containing your music files.
2. Set the destination directory (e.g. your SD card mount point).
3. Choose the target size and reserve percentage.
4. Click **Scan Library** (first run probes all files; subsequent runs load from cache).
5. Browse the tree and check/uncheck albums or artists. The budget bar updates in real time.
6. Click **Copy Selected** to copy the chosen tracks to the destination in a `Bucket/Artist/Album/NNN - Title.ext` layout.

## Downloads

Pre-built binaries for Windows, macOS, and Linux are available on the [Releases](https://github.com/rdapaz/MusicSync/releases) page.

## License

[MIT](LICENSE)
