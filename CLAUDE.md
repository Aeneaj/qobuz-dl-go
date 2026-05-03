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
internal/lyrics/     Descarga de .lrc: lector de metadatos FLAC/MP3, cliente LRCLIB
```

## Filosofía de Arquitectura

### Zero Dependencies para parseo de audio

El tagging y la lectura de metadatos FLAC y MP3 están implementados en **Go puro**, sin librerías externas de audio:

- `internal/downloader/metadata.go` — escritura de tags (Vorbis Comment + ID3v2.3)
- `internal/lyrics/metadata.go` — lectura de tags y duración (STREAMINFO, Vorbis Comment, ID3v2.3/v2.4, cabecera Xing, estimación CBR)

No añadir dependencias de parseo de audio externas. Si necesitas leer o escribir un campo nuevo de metadatos, impleméntalo en pure Go.

### UI de Terminal: siempre `mpb`

Todas las barras de progreso y el feedback visual se implementan con `github.com/vbauerster/mpb/v8`. Patrones establecidos:

- Estilo de barra: `╢█████░░░╟` (`Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟")`)
- Etiqueta izquierda (PrependDecorators): ancho fijo con `truncateStr` o `buildLabel`
- Etiqueta dinámica: `decor.Any(func(_ decor.Statistics) string {...})` + `atomic.Value` para thread safety
- Completado: `decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m")`
- Refresh: `mpb.WithRefreshRate(150 * time.Millisecond)`

Cualquier nueva feature con feedback visual debe reutilizar este patrón para consistencia.

## Dependencias externas

Las dependencias de módulo son:
- `github.com/vbauerster/mpb/v8` — barras de progreso
- `github.com/acarl005/stripansi` — limpieza de secuencias ANSI
- `github.com/VividCortex/ewma` — media móvil (usada por mpb)
- `github.com/mattn/go-runewidth` — ancho de caracteres Unicode
- `github.com/clipperhouse/uax29/v2` — segmentación de texto Unicode
- `golang.org/x/sys` — syscalls de bajo nivel

No añadir dependencias nuevas sin discusión. En particular no añadir librerías de parseo de audio (dhowden/tag, mewkiz/flac, bogem/id3v2, etc.) — ya tenemos implementaciones propias.

## Filosofía de Tests

### Reglas invariantes

- **Sin testify ni mocks externos.** Solo stdlib: `testing`, `net/http/httptest`, `os`, `io`, etc.
- **Tests rápidos y offline.** Ningún test hace peticiones reales a internet. Servidores mock con `httptest.NewServer`.
- **Table-driven por defecto.** Slice de `struct{ in, want }` + bucle `for _, c := range cases`. Subtests con `t.Run` cuando hay nombre descriptivo.
- **Inyección de dependencias por parámetro.** La función pública (`Run`) recibe un cliente real; la interna (`runWithClient`) acepta el cliente como argumento para que los tests pasen uno falso. No usar variables globales para el cliente.
- **Helpers de test mínimos.** Los constructores de datos falsos (`fakeFLAC`, `fakeMP3`) viven en `*_test.go`, no en producción.

### Cobertura por paquete

| Paquete | Tests | Cobertura | Archivos de test |
|---|---|---|---|
| api | — | ~42% | api_test.go |
| bundle | — | ~64% | bundle_test.go |
| config | — | ~46% | config_test.go |
| downloader | 30+ | ~35% | metadata_test.go, db_test.go, lastfm_test.go, helpers_test.go |
| lyrics | 42 | ~100% | metadata_test.go, lrclib_test.go, lyrics_test.go |

`helpers_test.go` en downloader cubre: `sanitize`, `expandPlaceholders`, `renderFormat`, `formatDuration`, `idStr`, `nestedStr`, `releaseYear`, `essenceTitle`, `isAlbumType`.

### CI (`.github/workflows/ci.yml`)

```yaml
- name: Format   # falla si algún archivo no está formateado con gofmt
  run: test -z "$(gofmt -l .)"
- name: Vet
  run: go vet ./...
- name: Test     # -cover imprime cobertura por paquete
  run: go test -cover ./...
```

### Checklist antes de añadir tests nuevos

1. Buscar si la función ya tiene tests en `*_test.go` del mismo paquete.
2. Preferir extender una tabla existente antes de crear nueva función de test.
3. Asegurarse de que `go fmt ./...` no cambia nada antes de commit.

## Estado actual (v1.4.0)

- `go build ./...` ✅
- `go vet ./...` ✅
- `go fmt ./...` ✅ (CI falla si hay archivos sin formatear)
- `go test -cover ./...` ✅ (todos los paquetes pasan)
- Cobertura: api 42%, bundle 64%, config 46%, downloader ~35% (30+ tests), lyrics 100% (42 tests)

## Comandos de construcción

```bash
go build -o qobuz-dl ./cmd/qobuz-dl/   # compilar binario
go build ./...                          # verificar que compila todo
go vet ./...                            # análisis estático
go test ./...                           # todos los tests
go test ./internal/lyrics/... -v        # tests de un paquete concreto
```

## Autenticación Qobuz (abril 2026)

Password auth rota (401). Workarounds implementados:
1. **Token**: `qobuz-dl --reset` → pegar user_id + user_auth_token desde DevTools
2. **OAuth** (recomendado): `qobuz-dl oauth` → servidor local captura redirect con `user_auth_token=` o `code_autorisation=`
3. `/oauth/callback` puede devolver 404 — el código intenta `code_autorisation` y `code` como fallback

### Flujo de inicialización y credenciales

`loadOrInitConfig(skipCredentials bool)` en `main.go` gestiona la primera ejecución:
- Si ya existe `config.ini` → lo carga directamente.
- Si NO existe y `skipCredentials=false` → llama `config.Reset()` (pide user_id + token + preferencias).
- Si NO existe y `skipCredentials=true` → llama `config.InitConfig()` (solo preferencias, deja credenciales vacías).

Callers:
- `initDownloader(...)` → `loadOrInitConfig(false)` — todos los comandos de descarga (dl, lucky, csv, fun).
- `runOAuth(...)` → `loadOrInitConfig(true)` — el flujo OAuth obtiene y guarda el token él mismo via `config.SaveToken`.
- `--reset` flag → llama `config.Reset()` directamente, sin pasar por `loadOrInitConfig`.
- `runLyrics(...)` → llama `config.Load()` directamente (solo necesita `DownloadDir`, no credenciales).

Funciones en `internal/config/config.go`:
- `Reset()` — setup completo con credenciales manuales. Solo para `--reset`.
- `InitConfig()` — setup sin credenciales. Solo para primera ejecución con `oauth`.
- `setupPreferences(kv)` — helper interno compartido por ambas: bundle fetch + prompts de directorio/calidad/formatos.

**Regla UX**: nunca pedir user_id/user_auth_token al usuario cuando el comando es `oauth` o `lyrics`. El token llega del flujo OAuth; `lyrics` no necesita Qobuz.

## Comandos

```bash
go build -o qobuz-dl ./cmd/qobuz-dl/
./qobuz-dl --reset           # configurar con token manual (pide user_id + token + preferencias)
./qobuz-dl oauth             # login OAuth (primera ejecución solo pide preferencias básicas)
./qobuz-dl dl <URL>          # descargar por URL
./qobuz-dl lucky -q 6 "Radiohead"  # búsqueda + descarga
./qobuz-dl fun               # modo interactivo
./qobuz-dl lyrics            # fetch .lrc para el directorio configurado
./qobuz-dl lyrics ~/Music    # fetch .lrc para una ruta específica
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

**Importante**: el comando `lyrics` usa `resolveScanDir` (en `lyrics_cmd.go`), que es igual a `ResolveDir` pero **sin** `os.MkdirAll` — no crea el directorio si no existe. El usuario debe apuntar a una biblioteca ya existente.

## Comando `lyrics` — detalles de implementación

### Paquete `internal/lyrics/`

```
metadata.go  — lectura de tags y duración desde FLAC y MP3 (pure Go)
lrclib.go    — cliente HTTP para LRCLIB API
lyrics.go    — orquestador: escaneo → barra mpb → fetch secuencial → escritura .lrc
```

### Lectura de metadatos (metadata.go)

**FLAC:**
- Bloque STREAMINFO (tipo 0): `sample_rate` (20 bits) + `total_samples` (36 bits) → `duration = total_samples / sample_rate`
- Bloque VORBIS_COMMENT (tipo 4): pares `KEY=VALUE` en UTF-8; `ARTIST` tiene prioridad sobre `ALBUMARTIST`

**MP3:**
- Cabecera ID3v2.3 y v2.4 (tamaño syncsafe para v2.4, BE uint32 para v2.3)
- Frames `TIT2`, `TPE1`, `TPE2`, `TALB`, `TLEN`; decodifica Latin-1, UTF-16LE/BE, UTF-8
- Duración: `TLEN` (ms) → cabecera Xing/Info (VBR, total_frames × spf / sr) → estimación CBR (filesize × 8 / bitrate)

### LRCLIB API (lrclib.go)

`GET https://lrclib.net/api/get?track_name=...&artist_name=...&album_name=...&duration=...`
- 200: prioriza `syncedLyrics` sobre `plainLyrics`
- 404: `("", nil)` — no es un error
- 429: `time.Sleep(retryDelay)` + un reintento
- `duration` se omite del query cuando es 0

Campos testables en `Client`: `baseURL`, `retryDelay`, `StepDelay` (todos configurables en tests para velocidad y mock).

### Orquestador (lyrics.go)

- `Run(dir string) error` — API pública, llama `runWithClient(dir, NewClient())`
- `runWithClient(dir string, client *Client) error` — función interna inyectable en tests
- Barra mpb con `decor.Any` + `atomic.Value` para etiqueta dinámica `[N/M] Título — Artista`
- `time.Sleep(client.StepDelay)` entre requests (500ms en producción, 0 en tests)
- Warnings (404, errores) acumulados en slice, impresos todos tras `p.Wait()`

### Tests (42 tests, cobertura completa)

```
metadata_test.go  — FLAC tags+duración, fallback ALBUMARTIST, MP3 Latin-1/UTF-16LE/TLEN/TPE2, decodeID3Text
lrclib_test.go    — syncedLyrics preferred, plainFallback, 404→nil, 429→error, queryParams, OmitsDuration, retry429
lyrics_test.go    — buildLabel (formato, ancho fijo, truncado), lrcPathFor, scanAudioFiles, runWithClient e2e
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
- [x] Descarga de letras sincronizadas — `internal/lyrics/`
      LRCLIB API pública (sin auth); prioriza syncedLyrics sobre plainLyrics; rate limiting 500ms/req;
      retry único en 429; skip si ya existe .lrc; barra mpb con etiqueta dinámica; zero-deps para parseo FLAC/MP3.
      Comando: `./qobuz-dl lyrics [ruta]`. Navidrome-compatible (Plug & Play karaoke).
