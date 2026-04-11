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
