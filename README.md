# qobuz-dl-go

Download your Qobuz music to your computer — albums, tracks, playlists, whole
discographies — in CD or Hi-Res quality, with cover art, correct tags and
optional synced lyrics.

A rewrite in Go of [vitiko98/qobuz-dl](https://github.com/vitiko98/qobuz-dl).
One self-contained binary, nothing else to install.

**You need:** a Qobuz account with an active subscription. Free accounts can
browse and search, but Qobuz will not let them download tracks.

---

## Quick start

### 1. Build it

You need [Go 1.24+](https://go.dev/dl/) installed. Then:

```bash
git clone https://github.com/Aeneaj/qobuz-dl-go.git
cd qobuz-dl-go
go build -o qobuz-dl ./cmd/qobuz-dl/
```

That leaves a single file called `qobuz-dl` in the folder. Every command below
is run from there as `./qobuz-dl ...`.

### 2. Log in

```bash
./qobuz-dl oauth
```

The first run asks three simple questions (where to save music, folder name,
audio quality — pressing Enter accepts the defaults), then opens Qobuz in your
browser. Log in there and you're done: the token is saved automatically and you
never have to do this again.

<details>
<summary>If the browser login doesn't work</summary>

Qobuz sometimes shows a 404 page after login. Copy the URL of that page from
the address bar and pass it in:

```bash
./qobuz-dl oauth "https://www.qobuz.com/...paste-the-whole-url..."
```

Still stuck? You can enter your credentials by hand with `./qobuz-dl --reset`.
It asks for a `user_id` and a `user_auth_token`, which you find like this:

1. Log in at [play.qobuz.com](https://play.qobuz.com) in your browser
2. Press F12 → **Application** tab → **Local Storage**
3. Open the `localuser` entry and copy the `id` and `userAuthToken` values

</details>

### 3. Download something

Copy an album URL from the Qobuz website and paste it after `dl`:

```bash
./qobuz-dl dl https://www.qobuz.com/us-en/album/.../abc123
```

Files land in the folder you chose during setup (by default `qobuz-downloader`
next to the binary), one folder per album:

```
Radiohead - In Rainbows (2007) [24B-96kHz]/
  01. 15 Step.flac
  02. Bodysnatchers.flac
  cover.jpg
```

That's it. Everything below is optional.

### Don't like typing commands?

Then skip them. This opens the whole program as a menu:

```bash
./qobuz-dl tui
```

Search, queue things up, download, fetch lyrics, change settings — all with the
arrow keys, no commands to remember. It's the same program, just a different
way in, and you can log in from the menu too. See [Full-screen
mode](#full-screen-mode) for what's on each screen.

One catch: it needs a real terminal window. If you run it from a script or a
cron job it will stop with `could not open a new TTY` — use the regular
commands there.

---

## One rule about options

⚠️ **Options always go before the command**, never after it:

```bash
./qobuz-dl -q 27 dl https://...        ✅ works
./qobuz-dl dl https://... -q 27        ❌ silently ignored
```

This trips everybody up once. If a setting seems to have no effect, this is
almost certainly why. (The one exception is `lyrics`, which accepts `-d` after
the command too.)

---

## What you can download

Paste any of these URLs after `dl` — one at a time or several at once:

| You paste | You get |
|---|---|
| An album URL | The whole album |
| A track URL | That single track |
| An artist URL | That artist's full discography |
| A label URL | Everything from that label |
| A Qobuz playlist URL | Every track in the playlist |
| `last.fm/user/NAME/loved` | Your Last.fm loved tracks, searched on Qobuz |
| `last.fm/user/NAME/library` | Your recent Last.fm tracks, searched on Qobuz |

```bash
# Several at once
./qobuz-dl dl https://www.qobuz.com/album/... https://www.qobuz.com/track/...
```

Don't have a URL? Search from the terminal instead:

```bash
# Download the best-matching album for a search
./qobuz-dl lucky "Radiohead In Rainbows"

# Download the top 3 matching albums
./qobuz-dl --lucky-n 3 lucky "Radiohead"

# Search for tracks instead of albums
./qobuz-dl --lucky-type track lucky "Paranoid Android"
```

---

## Common tasks

```bash
# Best available quality (Hi-Res 24-bit)
./qobuz-dl -q 27 dl https://www.qobuz.com/album/...

# Save somewhere else, just this once
./qobuz-dl -d ~/Music dl https://www.qobuz.com/album/...

# Put the cover art inside the audio files (for players that want it embedded)
./qobuz-dl --embed-art dl https://www.qobuz.com/album/...

# Grab an artist's discography, skipping singles and EPs
./qobuz-dl --albums-only dl https://www.qobuz.com/interpreter/...

# ...and also skip live albums and compilations
./qobuz-dl --albums-only --smart-discog dl https://www.qobuz.com/interpreter/...

# Faster: 6 tracks downloading at the same time instead of 3
./qobuz-dl --workers 6 dl https://www.qobuz.com/album/...

# Full-screen view with per-track bars, speed and ETA (works with dl, lucky, csv)
./qobuz-dl --tui dl https://www.qobuz.com/album/...
```

### Audio quality

| Use `-q` | What it is |
|---|---|
| `5` | MP3 320 kbps |
| `6` | FLAC, CD quality (16-bit / 44.1 kHz) — **default** |
| `7` | Hi-Res, up to 24-bit / 96 kHz |
| `27` | Hi-Res, above 96 kHz — the maximum |

Ask for a quality the album doesn't have and you automatically get the next
best one instead, so `-q 27` is a safe default for "give me the best there is".
Use `--no-fallback` if you'd rather it skip the album than downgrade.

---

## Full-screen mode

```bash
./qobuz-dl tui
```

That opens the whole program in one screen — no commands to remember. Arrow
keys move, Enter chooses, Esc goes back, `q` quits:

```
╭──────────────────────────────────────────────────────────────────╮
│ ◈ QOBUZ-DL                             ○ sin sesión  │  cola 0   │
╰──────────────────────────────────────────────────────────────────╯

  BUSCAR
   ♫  Buscar álbumes  ·  busca en Qobuz y añade a la cola
   ♪  Buscar canciones
   ◈  Buscar artistas
   ≡  Buscar playlists

  COLA
   +  Añadir URL
   ▤  Ver la cola
   ⬇  Descargar la cola

  HERRAMIENTAS
   ♬  Letras (.lrc)
   ⇪  Importar CSV
   ⚙  Configuración
   ✖  Borrar historial

  SESIÓN
   ⚿  Iniciar sesión (OAuth)
   ⏻  Salir

────────────────────────────────────────────────────────────────────
  ↑↓ moverse   ⏎ elegir   esc volver   q salir
```

The indicator top-right tells you where you stand: `○ sin sesión` until you log
in, and how many items are queued. The highlighted row explains itself in the
line next to it.

Search results are a checklist: `space` marks the ones you want, `Enter` adds
them to the queue. Queue up as much as you like from as many searches as you
like, then pick **Descargar la cola** and watch the per-track bars. `Ctrl+C`
during a download cancels that download and returns you to the menu; from the
menu it exits.

**Logging in** is in the menu. Picking it drops out of the full-screen view,
runs the ordinary OAuth flow — the one that prints a URL and waits for your
browser — then returns to the menu with your session live. The screen flips out
and back; that is expected. `./qobuz-dl oauth` from the command line still works
and does exactly the same thing.

You can open `tui` without being logged in: the menu is where the login lives,
so it would be no help to refuse to start. Anything needing Qobuz says so in
the footer until you log in. Fetching lyrics works either way — LRCLIB is
public.

## Interactive mode (line based)

`./qobuz-dl fun` is the older text prompt, still there if you prefer typing
commands to navigating menus:

```
qobuz > sa radiohead          search albums
qobuz > st paranoid android   search tracks
qobuz > sr radiohead          search artists
qobuz > sp workout            search playlists
qobuz > dl https://...        add a URL to the list
qobuz > q                     show the list
qobuz > rm 2                  remove item 2
qobuz > go                    download everything
qobuz > exit                  quit
```

After a search, type the numbers of the results you want (e.g. `1 3 5`) to add
them to the list. `help` shows every command.

---

## Synced lyrics

Adds karaoke-style `.lrc` lyrics files to a music library, so players like
Navidrome, Jellyfin or Plex can show lyrics scrolling in time with the song.

```bash
# Your qobuz-dl download folder
./qobuz-dl lyrics

# Any other music folder
./qobuz-dl lyrics ~/Music
```

It scans every FLAC and MP3 in the folder and its subfolders, and writes a
lyrics file next to each song (`01. Song.flac` → `01. Song.lrc`).

Worth knowing:

- **Works on any music**, not just Qobuz downloads — it doesn't even need you
  to be logged in. Lyrics come from the free [LRCLIB](https://lrclib.net)
  database.
- **Safe to re-run.** Songs that already have a lyrics file are skipped
  instantly, so you can run it again after every new download.
- **Not every song has lyrics.** Missing ones are listed at the end; the rest
  still get theirs.
- **The folder must already exist** — unlike downloading, this command won't
  create it.

---

## Importing playlists from Spotify, Apple Music, etc.

Use the free [TuneMyMusic](https://www.tunemymusic.com/) to export any playlist
as a CSV file, then hand that file to qobuz-dl:

```bash
./qobuz-dl csv my_playlist.csv

# Hi-Res, and write down anything that couldn't be downloaded
./qobuz-dl -q 27 --failed skipped.csv csv my_playlist.csv
```

Each song is searched on Qobuz and downloaded. At the end you get a summary:

```
=== CSV Batch Summary ===
  Total processed: 50
  Downloaded:      45
  Not found:        3
  Errors:           2
  Failed tracks saved to: skipped.csv
```

The `--failed` file lists what didn't work and why, so you can look those few
up by hand.

---

## Troubleshooting

**"Free accounts are not eligible to download tracks"**
Your Qobuz plan doesn't include downloads. A paid subscription is required —
this isn't something the tool can work around.

**Login stopped working / "unauthorized" errors**
Tokens expire. Run `./qobuz-dl oauth` again.

**A flag seems to do nothing**
It's probably placed after the command. See [the rule above](#one-rule-about-options).

**Nothing downloads, everything is "skipped"**
qobuz-dl remembers what it already downloaded and won't fetch it twice. To
force it:

```bash
./qobuz-dl --no-db dl https://...   # ignore the memory this once
./qobuz-dl --purge                  # forget everything permanently
```

**Where did my files go? / What are my settings?**

```bash
./qobuz-dl --show-config
```

**I want to start over**
`./qobuz-dl --reset` re-runs the whole setup.

---

## Settings

Your answers from the first run are saved in a config file you can edit by hand:

- **Linux / macOS**: `~/.config/qobuz-dl/config.ini`
- **Windows**: `%APPDATA%\qobuz-dl\config.ini`

Run `./qobuz-dl --show-config` to see where it is and what's in it, or
`./qobuz-dl --reset` to answer the setup questions again.

### Renaming folders and files

Folder and file names are built from a template. The defaults:

```
folder_format = {artist} - {album} ({year}) [{bit_depth}B-{sampling_rate}kHz]
track_format  = {tracknumber}. {tracktitle}
```

Which produces `Radiohead - In Rainbows (2007) [24B-96kHz]/01. 15 Step.flac`.
Change them in the config file, or per run with `--folder-format` /
`--track-format`. Available pieces:

`{artist}` `{album}` `{year}` `{bit_depth}` `{sampling_rate}` `{tracknumber}`
`{tracktitle}` `{genre}` `{composer}`

### Where files are saved

In order of priority: the `-d` flag → `download_dir` in the config file →
a `qobuz-downloader` folder next to the binary.

---

## All options

Remember: these go **before** the command.

| Flag | What it does |
|---|---|
| `-d <dir>` | Save to this folder instead of the configured one |
| `-q <5\|6\|7\|27>` | Audio quality (see table above) |
| `--embed-art` | Put cover art inside the audio files |
| `--og-cover` | Download the cover at maximum resolution |
| `--no-cover` | Don't download cover art at all |
| `--albums-only` | Skip singles and EPs |
| `--smart-discog` | Also skip live albums and compilations |
| `--no-m3u` | Don't create `.m3u` playlist files |
| `--no-fallback` | Skip albums rather than downloading a lower quality |
| `--workers N` | How many tracks to download at once (default 3) |
| `--tui` | Full-screen download view for `dl`/`lucky`/`csv` (see also the `tui` command) |
| `--no-db` | Re-download even things you already have |
| `--folder-format` | Album folder name template |
| `--track-format` | Track file name template |
| `--lucky-type` | What `lucky` searches: `album`, `track`, `artist`, `playlist` |
| `--lucky-n` | How many results `lucky` downloads (default 1) |
| `--failed <file>` | For `csv`: save the failures to this file |
| `-r`, `--reset` | Re-run setup |
| `-s`, `--show-config` | Show settings and where they live |
| `-p`, `--purge` | Forget the download history |
| `-v`, `--version` | Print the version |

### Commands

| Command | What it does |
|---|---|
| `dl <URL...>` | Download one or more URLs |
| `lucky <words>` | Search, then download the top result(s) |
| `tui` | Full-screen interface: menu, search, queue, downloads, lyrics |
| `fun` | Line-based interactive search-and-download mode |
| `csv <file>` | Download a playlist exported from TuneMyMusic |
| `lyrics [folder]` | Fetch `.lrc` lyrics for a music library |
| `oauth [url\|code]` | Log in |

---

## For developers

Pure Go, no cgo, no external tools like ffmpeg. FLAC (Vorbis Comment) and MP3
(ID3v2.3) tagging are implemented in-repo; audio parsing dependencies are
deliberately avoided. The dependency list is stdlib plus progress bars
(`mpb`) and its Unicode/ANSI helpers.

```
cmd/qobuz-dl/        CLI entry point
internal/api/        Qobuz HTTP API client
internal/bundle/     Scraper for app_id / secrets / private_key from bundle.js
internal/config/     INI config reader/writer
internal/downloader/ Download logic, FLAC/MP3 tagging, collections, OAuth
internal/lyrics/     .lrc fetcher: audio metadata reader (FLAC/MP3), LRCLIB client
```

```bash
go build ./...
go vet ./...
go test -cover ./...
```

See [CLAUDE.md](CLAUDE.md) for architecture notes and testing conventions.

## Credits

Based on [vitiko98/qobuz-dl](https://github.com/vitiko98/qobuz-dl) and its OAuth
PR [#331](https://github.com/vitiko98/qobuz-dl/pull/331). All credit for the
original design and reverse engineering goes to the upstream project and its
contributors.

## License

See [LICENSE](LICENSE).
