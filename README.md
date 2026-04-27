# qobuz-dl-go

A complete rewrite of [vitiko98/qobuz-dl](https://github.com/vitiko98/qobuz-dl) in Go. Downloads music from Qobuz — albums, tracks, playlists, artists, and labels — with support for lossless and hi-res audio.

## Features

- Download albums, tracks, artists, playlists, and labels by URL
- Quality up to 24-bit / 192kHz Hi-Res
- FLAC (Vorbis Comment) and MP3 (ID3v2.3) tagging — pure Go, no external tools needed
- Cover art download and optional embedding
- M3U playlist generation
- Concurrent track downloads per album
- Downloads database to skip already-downloaded tracks
- Configurable folder and track naming formats
- OAuth and manual token authentication (password login no longer supported by Qobuz)
- CSV batch download from [TuneMyMusic](https://www.tunemymusic.com/) exports
- Small dependency footprint — stdlib plus a handful of focused libraries (progress bars, Unicode, ANSI)

## Requirements

- Go 1.24+
- A Qobuz account (subscription required for lossless quality)

## Build

```bash
git clone https://github.com/Aeneaj/qobuz-dl-go.git
cd qobuz-dl-go
go build -o qobuz-dl ./cmd/qobuz-dl/
```

## Authentication

Password-based login is no longer accepted by Qobuz. Use one of these methods instead:

### Option 1 — OAuth (recommended)

```bash
./qobuz-dl oauth
```

Opens a local server that captures the OAuth redirect and saves your token automatically. If the browser redirect fails (404), you can paste the redirect URL or authorization code:

```bash
./qobuz-dl oauth <redirect-url-or-code>
```

### Option 2 — Manual token

```bash
./qobuz-dl --reset
```

Prompts for `user_id` and `user_auth_token`. To find them:
1. Log in at [play.qobuz.com](https://play.qobuz.com)
2. Open DevTools → Application → Local Storage
3. Find the `localuser` key and copy `id` and `userAuthToken`

## Usage

```
qobuz-dl [options] <command> [args]
```

### Commands

| Command | Description |
|---|---|
| `dl <URL...>` | Download one or more URLs (album, track, artist, label, playlist, Last.fm) |
| `lucky <query>` | Search and download the top N results |
| `oauth [code\|url]` | Log in via OAuth |
| `csv <file>` | Batch download from a TuneMyMusic CSV export |
| `fun` | Interactive search and download mode |

### Examples

```bash
# Download an album by URL
./qobuz-dl dl https://www.qobuz.com/album/...

# Download multiple URLs at once
./qobuz-dl dl https://www.qobuz.com/album/... https://www.qobuz.com/track/...

# Download in hi-res quality
./qobuz-dl -q 27 dl https://www.qobuz.com/album/...

# Download to a specific directory
./qobuz-dl -d ~/Music dl https://www.qobuz.com/album/...

# Download your Last.fm loved tracks (searches each on Qobuz)
./qobuz-dl dl https://www.last.fm/user/yourusername/loved

# Download your Last.fm recent tracks
./qobuz-dl dl https://www.last.fm/user/yourusername/library

# Search and download top 3 albums by an artist
./qobuz-dl lucky --lucky-n 3 "Radiohead"

# Search for tracks instead of albums
./qobuz-dl lucky --lucky-type track "Paranoid Android"

# Interactive REPL mode
./qobuz-dl fun
```

### Interactive mode (`fun`)

A command-driven REPL for searching and building a download queue without leaving the session:

```
qobuz > sa radiohead          # search albums
qobuz > st paranoid android   # search tracks
qobuz > sr radiohead          # search artists
qobuz > sp workout            # search playlists
qobuz > dl https://...        # add a URL directly to the queue
qobuz > q                     # show the queue
qobuz > rm 2                  # remove item 2 from the queue
qobuz > go                    # start downloading
qobuz > exit                  # quit
```

After each search, pick result numbers to add to the queue (e.g. `1 3 5`). Type `help` for the full command list.

### Last.fm playlists

Pass a Last.fm user playlist URL to `dl` and qobuz-dl will fetch the track list and search each song on Qobuz automatically:

| URL | What it downloads |
|---|---|
| `https://www.last.fm/user/{user}/loved` | Your loved tracks |
| `https://www.last.fm/user/{user}/library` | Your recent tracks |

No Last.fm API key is required. Tracks are saved to `<download-dir>/Last.fm - {user} - {type}/`. Tracks not found on Qobuz are skipped and counted in the final summary.

### CSV batch download (`csv`)

Export a playlist from [TuneMyMusic](https://www.tunemymusic.com/) as CSV, then:

```bash
# Basic batch download
./qobuz-dl csv my_playlist.csv

# Hi-res quality + save a report of tracks that failed or weren't found
./qobuz-dl csv my_playlist.csv -q 27 --failed skipped.csv

# Download to a specific folder
./qobuz-dl -d ~/Music csv my_playlist.csv -q 6
```

The parser handles the most common TuneMyMusic export quirk: when `Artist name`, `Album`, and `ISRC` columns are blank and the track appears as `"Artist - Title"` in the `Track name` field, artist and title are inferred automatically.

At the end of the run a summary is printed:

```
=== CSV Batch Summary ===
  Total processed: 50
  Downloaded:      45
  Not found:        3
  Errors:           2
  Failed tracks saved to: skipped.csv
```

The `--failed` report is a CSV with columns `row`, `artist`, `title`, `query`, `reason` — inspect it and retry manually.

### Options

| Flag | Description |
|---|---|
| `-r`, `--reset` | Reconfigure credentials |
| `-s`, `--show-config` | Show config file path and current contents |
| `-p`, `--purge` | Delete the downloads database |
| `-d <dir>` | Download directory (overrides config) |
| `-q <quality>` | Audio quality (see table below) |
| `--embed-art` | Embed cover art into audio files |
| `--albums-only` | Skip singles and EPs |
| `--no-m3u` | Do not create M3U playlist files |
| `--no-fallback` | Disable automatic quality fallback |
| `--og-cover` | Download original (max) resolution cover art |
| `--no-cover` | Skip cover art download |
| `--no-db` | Bypass the downloads database (re-download everything) |
| `--workers N` | Number of concurrent track downloads per album (default: 3) |
| `--folder-format` | Folder naming format string |
| `--track-format` | Track naming format string |
| `--smart-discog` | Smart discography filter (skip live/compilation albums) |
| `--lucky-type` | Item type for `lucky` command: `album`, `track`, `artist`, `playlist` |
| `--lucky-n` | Number of results to download with `lucky` (default: 1) |
| `--failed <file>` | (`csv` only) Save undownloaded tracks to this CSV file |

### Quality levels

| Value | Description |
|---|---|
| `5` | MP3 320 kbps |
| `6` | FLAC 16-bit / 44.1 kHz (CD quality, default) |
| `7` | Hi-Res 24-bit / up to 96 kHz |
| `27` | Hi-Res 24-bit / above 96 kHz |

If the requested quality is unavailable, the downloader falls back to the next available tier automatically (disable with `--no-fallback`).

## Configuration

The config file is created automatically on first run at:

- **Linux / macOS**: `~/.config/qobuz-dl/config.ini`
- **Windows**: `%APPDATA%\qobuz-dl\config.ini`

Re-run setup at any time with `./qobuz-dl --reset`.

### Naming formats

Folder and track names are configurable with format strings. Default values:

```
folder_format = {artist} - {album} ({year}) [{bit_depth}B-{sampling_rate}kHz]
track_format  = {tracknumber}. {tracktitle}
```

Available tokens: `{artist}`, `{album}`, `{year}`, `{bit_depth}`, `{sampling_rate}`, `{tracknumber}`, `{tracktitle}`, `{genre}`, `{composer}`.

## Downloads database

By default qobuz-dl keeps a plain-text database of downloaded track IDs at `~/.config/qobuz-dl/qobuz_dl.db` so that already-downloaded tracks are skipped on future runs.

```bash
./qobuz-dl --no-db dl <URL>   # skip the database for this run
./qobuz-dl --purge            # delete the database entirely
```

## Project structure

```
cmd/qobuz-dl/        CLI entry point
internal/api/        Qobuz HTTP API client
internal/bundle/     Scraper for app_id / secrets / private_key from bundle.js
internal/config/     INI config reader/writer
internal/downloader/ Download logic, FLAC/MP3 tagging, collections, OAuth
```

## Credits

Based on [vitiko98/qobuz-dl](https://github.com/vitiko98/qobuz-dl) and its OAuth PR [#331](https://github.com/vitiko98/qobuz-dl/pull/331). All credit for the original design and reverse engineering goes to the upstream project and its contributors.

## License

See [LICENSE](LICENSE).
