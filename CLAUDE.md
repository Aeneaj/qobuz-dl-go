# qobuz-dl-go

Traducción completa a Go del PR #331 de vitiko98/qobuz-dl (autenticación OAuth).
Original Python: https://github.com/vitiko98/qobuz-dl/pull/331

## Estructura

```
cmd/qobuz-dl/        CLI entry point (flag stdlib, sin dependencias externas)
internal/api/        Cliente HTTP Qobuz API (qopy.py)
internal/bundle/     Scraper de app_id/secrets/private_key de bundle.js
internal/config/     Lector/escritor de config.ini (INI casero, sin deps)
internal/downloader/ Descarga, tagging FLAC/MP3, colecciones, OAuth
```

## Dependencias externas

El tagging FLAC (Vorbis Comment) y MP3 (ID3v2.3) están implementados en Go puro
en `internal/downloader/metadata.go`, sin herramientas externas del sistema.
Las dependencias de módulo son:
- `github.com/vbauerster/mpb/v8` — barras de progreso
- `github.com/acarl005/stripansi` — limpieza de secuencias ANSI
- `github.com/VividCortex/ewma` — media móvil (usada por mpb)
- `github.com/mattn/go-runewidth` — ancho de caracteres Unicode
- `github.com/clipperhouse/uax29/v2` — segmentación de texto Unicode
- `golang.org/x/sys` — syscalls de bajo nivel

## Estado actual

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./...` ✅ (todos los paquetes pasan)
- Cobertura: api 42%, bundle 64%, config 46%, downloader 25%

## Autenticación Qobuz (abril 2026)

Password auth rota (401). Workarounds implementados:
1. **Token** (recomendado): `qobuz-dl --reset --token` → pegar user_id + user_auth_token desde DevTools
2. **OAuth**: `qobuz-dl oauth` → servidor local captura redirect con `user_auth_token=` o `code_autorisation=`
3. `/oauth/callback` puede devolver 404 — el código intenta `code_autorisation` y `code` como fallback

## Comandos

```bash
go build -o qobuz-dl ./cmd/qobuz-dl/
./qobuz-dl --reset --token        # configurar con token
./qobuz-dl oauth                   # login OAuth
./qobuz-dl dl <URL>               # descargar por URL
./qobuz-dl lucky -q 6 "Radiohead" # búsqueda + descarga
./qobuz-dl fun                     # modo interactivo
```

## Directorio de descarga (`download_dir`)

Jerarquía de prioridad al resolver la ruta de descarga:
1. Flag CLI `-d <ruta>` (máxima prioridad)
2. Clave `download_dir` en `config.ini`
3. Fallback: `./qobuz-downloader` (relativo al CWD)

Implementación:
- `config.ResolveDir(dir string) (string, error)` — expande `~`, llama `filepath.Abs`, crea con `os.MkdirAll`; devuelve error descriptivo si hay problema de permisos (sin panic)
- `Config.DownloadDir` — campo separado de `DefaultFolder` (que es el formato de nombre de álbum, no una ruta)
- `Reset()` pregunta al usuario por el directorio antes de `default_folder`
- `downloader.New()` ya no tiene fallback hardcodeado — la ruta llega siempre resuelta desde `initDownloader`

## Pendiente / Ideas

- [x] Downloads DB (archivo plano, un track ID por línea) — `internal/downloader/db.go`
      `--no-db` bypass; `--purge` borra el archivo; se carga al arrancar en un map[string]struct{}
- [x] Descargas concurrentes por track — semáforo + WaitGroup, flag `--workers N` (default 3)
- [ ] Tests de integración con servidor mock completo para downloader
- [x] Soporte last.fm playlists — `internal/downloader/lastfm.go`
      XSPF API 1.0 (sin API key); soporta `/user/{user}/loved` y `/user/{user}/library`;
      busca cada track en Qobuz y descarga el primer resultado
- [x] Modo interactivo mejorado — `internal/downloader/interactive.go`
      REPL con comandos: sa/st/sr/sp (búsqueda por tipo), dl (URL directa),
      q (ver queue), rm N (quitar item), clear, go (descargar), exit
- [x] Sistema de descarga robusto con reintentos — `internal/downloader/downloader.go`
      Motivación: fallos mid-download en FLACs grandes por drops del servidor (io.ErrUnexpectedEOF / net.Error).
      `downloadWithProgress` reescrito: hasta 5 reintentos con backoff exponencial (1s/2s/4s/8s),
      resume desde offset en disco via `Range: bytes=N-`, append al archivo parcial en vez de sobrescribir,
      bar fast-forward a bytes ya descargados via `barCredited`, maneja servidores que ignoran Range
      (responden 200 en vez de 206): trunca y reinicia limpio, cierra `resp.Body` explícitamente
      cada intento para no filtrar conexiones. Helpers: `isContextError`, `isRecoverableErr`.
