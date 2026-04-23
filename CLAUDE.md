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

## Sin dependencias externas

Solo stdlib de Go. El tagging FLAC (Vorbis Comment) y MP3 (ID3v2.3) están
implementados en Go puro en `internal/downloader/metadata.go`.

## Estado actual (v1.3.0)

- `go build ./...` ✅
- `go vet ./...` ✅
- `go test ./...` ✅ (todos los paquetes pasan)
- Cobertura: api 42%, bundle 64%, config 46%, downloader 25%
- Rama de release pendiente de PR: `release/v1.3.0` (push hecho, `gh` CLI no instalado en la máquina)

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
./qobuz-dl csv playlist.csv -q 6  # descarga por lotes desde CSV de TuneMyMusic
./qobuz-dl csv playlist.csv --failed skipped.csv  # con reporte de fallidos
```

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
- [x] Descarga por lotes desde CSV — `internal/downloader/csvbatch.go`
      Comando `csv <archivo.csv>` compatible con exportaciones de TuneMyMusic.
      `ParseCSV`: strip de BOM UTF-8, mapeo dinámico de columnas por nombre, inferencia de
      artista/título cuando vienen vacíos (split por ` - ` en Track name), FieldsPerRecord=-1.
      `DownloadCSV`: loop resiliente (nunca fatal), reutiliza `searchFirstTrackID` + `downloadTrackByID`,
      reporte de resumen al final, flag `--failed <file>` escribe CSV de canciones no encontradas/fallidas.

## Bugs críticos resueltos (v1.3.0)

- **Deadlock en descargas individuales** — `downloadWithProgress` llamaba `mpb.Bar.SetTotal(n, false)`:
  el `false` impide que mpb marque la barra como completa aunque llegue al 100%, dejando `p.Wait()`
  bloqueado para siempre. Fix: `SetTotal(n, false)` durante init (solo display), luego
  `SetTotal(completedAt, true)` explícito DESPUÉS de que `io.Copy` retorna, cuando `ProxyReader`
  ya no está activo.
- **Panic (nil pointer dereference en io.Copy) en reintentos** — dos bugs combinados:
  1. El bloque "server ignored Range" cerraba `resp.Body` y reseteaba estado pero no hacía `continue`,
     cayendo al resto de la iteración con un body ya cerrado.
  2. Con `triggerComplete=true` en `SetTotal`, mpb auto-completaba la barra mientras `io.Copy` seguía
     corriendo; el goroutine `serve` cerraba `operateState`, y el `IncrBy` del probe de EOF enviaba
     en canal cerrado → panic.
  Fix: `continue` en el bloque restart + `SetTotal(false)` durante init.
