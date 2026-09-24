# qobuz-dl-go

Traducción a Go de [vitiko98/qobuz-dl](https://github.com/vitiko98/qobuz-dl) con la
autenticación OAuth del PR #331. Descarga de Qobuz en FLAC/MP3 con tagging propio, TUI
opcional y descarga de letras desde LRCLIB.

## Comandos

```bash
go build -o qobuz-dl ./cmd/qobuz-dl/   # binario
go vet ./...
go test -race ./...                    # como CI: siempre con -race
gofmt -l .                             # debe salir vacío

./qobuz-dl oauth                       # login (recomendado)
./qobuz-dl --reset                     # config con user_id + token pegados a mano
./qobuz-dl dl <URL>                    # descargar por URL
./qobuz-dl lucky -q 6 "Radiohead"      # buscar y descargar el primero
./qobuz-dl fun                         # REPL interactivo
./qobuz-dl tui                         # todo el programa en pantalla completa
./qobuz-dl lyrics [ruta]               # .lrc para una biblioteca existente
```

## Estructura

```
cmd/qobuz-dl/        CLI (flag de stdlib)
  main.go            usage, main() como dispatcher, handlers cortos
  flags.go           cliFlags, registerDownloadFlags, loadOrInitConfig, initDownloader
  oauth_cmd.go       runOAuth / oauthLogin
  lyrics_cmd.go      runLyrics
  tui_cmd.go         tuiBackend: la implementación de ui.Backend
internal/api/        cliente HTTP de Qobuz
internal/bundle/     scraper de app_id/secrets de bundle.js
internal/config/     config.ini (INI propio, sin deps)
internal/downloader/
  downloader.go      Downloader, New, progreso (termOut/newBar/newProgress), HandleURL
  collection.go      artista/playlist/label, smartDiscogFilter
  album.go           downloadAlbum → collectTrackJobs → runTrackJobs
  track.go           una pista: finalTrackPath, alreadyHave, downloadAndTag, fallbackQuality
  transfer.go        downloadWithProgress: reintentos, resume por Range, stall timeout
  metadata.go        escritura de tags FLAC (Vorbis) y MP3 (ID3v2.3)
  search.go          Search / SearchURLs
  csvbatch.go        comando csv
  interactive.go     REPL de fun
  oauth.go           flujo OAuth
  db.go              DB de pistas descargadas (un id por línea)
  helpers.go         M3U, sanitize, safeJoin, validateFormats, limitNameBytes
internal/lyrics/     lectura de tags FLAC/MP3 + cliente LRCLIB + orquestador
internal/ui/         TUI bubbletea
  backend.go         interfaz Backend (rompe el ciclo de imports)
  shell.go           menú, búsqueda, cola, config (comando tui)
  model.go           pantalla de progreso (flag --tui)
  handle.go          TrackHandle, implementa downloader.ProgressBar
  lang.go            i18n: T() + mapa es
  widgets.go         textField y picker hechos a mano
  styles.go          paleta lipgloss
```

## Reglas rápidas

- **Dependencias directas: `mpb`, `bubbletea`, `lipgloss`.** Nada nuevo sin discutirlo. Nunca
  librerías de audio (dhowden/tag, mewkiz/flac, bogem/id3v2…): el parseo es propio, en Go puro.
- **CLI con `flag` de stdlib**, nada de cobra ni urfave/cli.
- **Tests solo con stdlib**, sin testify ni mocks externos, offline (`httptest`), table-driven.
- **Nada escribe a stdout ni stderr mientras hay barras o TUI** (ver "Salida a terminal").
- **Medir antes de optimizar o de afirmar.** Varias veces aquí el instinto obvio falló y
  solo lo aclaró una medición.

## Arquitectura

### Audio: Go puro, y el audio nunca pasa por la RAM

Tagging (`downloader/metadata.go`) y lectura de tags (`lyrics/metadata.go`) son propios.

Etiquetar o leer cuesta lo que ocupan los **metadatos**, no la pista:
- `writeFLACMeta` y `writeID3v23` leen solo la cabecera, la reescriben en memoria y
  `replaceHead` copia el audio de fichero a fichero (`io.Copy` entre `*os.File`). Escribe a
  `<ruta>.tag` y renombra al final, así que un fallo a mitad no deja la pista truncada.
- La portada va como trozo propio, sin copiarse. Una portada de más de 16 MB no se incrusta:
  la longitud del bloque FLAC es de 24 bits y se desbordaría.
- `lyrics` para en el último bloque FLAC; en MP3 `readID3Frames` lee solo
  TIT2/TPE1/TPE2/TALB/TLEN y salta el resto con `Seek`.

Resultado medido: un álbum de 12×50 MB con 3 workers pasó de 321 MB de RSS a 14,6 MB. Lo
guardan `TestTaggingMemoryIndependentOfTrackSize`, `TestReadFLACMemoryIndependentOfFileSize`,
`TestReadMP3MemoryIndependentOfCover` y `TestTagFLACSkipsOversizedCover`; los benchmarks
`BenchmarkMem*` de los `mem_test.go` dan las cifras.

Misma regla en las colecciones: `collectionIDs` devuelve solo ids, y mientras llegan las
páginas `slimPage` guarda de cada item solo `collectionFields`. **Si `smartDiscogFilter`
empieza a leer un campo nuevo, añádelo a `collectionFields`**
(`TestSlimPageKeepsWhatSmartDiscogReads` solo cubre los campos que ya conoce).

### Salida a terminal: `mpb` por defecto, TUI opcional

Por defecto se muestran barras `mpb`; `--tui` las cambia por la pantalla bubbletea. **Nunca
conviven**: con TUI, `newProgress` devuelve `nil` y `mpb` no se crea. El código de descarga
solo ve la interfaz `ProgressBar`: `*mpb.Bar` la cumple y `ui.TrackHandle` también. Un
display nuevo = implementar esos métodos + una rama en `newBar`/`newProgress`.

`TrackHandle` **no manda un mensaje por cada `Read`**: acumula bytes en un `atomic.Int64` que
el modelo lee cada tick de 100 ms. Un `p.Send()` por lectura satura bubbletea con 6 workers.
Lo guarda `TestTrackHandleReadSendsNothing`.

**Nada a stdout/stderr con barras vivas o TUI activa.** `mpb` repinta el cursor cada 150 ms y
la alt-screen de bubbletea se traga todo. Cómo escribir:
- En `downloader`, siempre `fmt.Fprintf(d.termOut(), ...)`. Devuelve el `*mpb.Progress` activo
  (serializa contra el render), `io.Discard` con TUI, o `os.Stdout` si no hay nada. Las
  funciones libres (`makeM3U`, `tagFLAC`, `cleanFormatStr`, `printBatchSummary`) reciben el
  `io.Writer` por parámetro.
- Un resumen se acumula y se imprime después de `p.Wait()` (así lo hace `lyrics`).
- Un error que el usuario debe ver **se devuelve**, no se imprime. Bajo TUI sale en la
  línea de estado; en CLI acaba en `fatalf`.
- Excepciones: `interactive.go` y `oauth.go` (tienen la terminal para ellos). Están en la
  allowlist de `TestNoDirectStdoutWrites`, que escanea el paquete. Esta regla se rompió ocho
  veces antes de que ese test existiera.

Estilo de las barras `mpb` (reutilizarlo en cualquier feedback visual nuevo):
- barra `╢█████░░░╟`: `Lbound("╢").Filler("█").Tip("█").Padding("░").Rbound("╟")`
- etiqueta izquierda de ancho fijo (`truncateStr` / `buildLabel`)
- etiqueta dinámica con `decor.Any` + `atomic.Value`
- completado: `decor.OnComplete(decor.Name(""), " \033[32m✓\033[0m")`
- `mpb.WithRefreshRate(150 * time.Millisecond)`

### TUI completa (`tui`)

`qobuz-dl tui` mete todo el programa en una pantalla. `--tui` es otra cosa: solo cambia el
display de progreso de `dl`/`lucky`/`csv`. Los dos comparten `Model`.

- **Ciclo de imports**: `downloader` importa `ui`, así que el shell no puede importar
  `downloader`. Por eso existe `ui.Backend`, implementada por `tuiBackend`. Una función nueva
  en el menú = método nuevo en `Backend` y en el adaptador.
- **OAuth suspende la TUI**: `tuiBackend.Login` hace `p.ReleaseTerminal()`, corre el flujo
  CLI y `RestoreTerminal()`. Hace falta porque `captureOAuthRedirect` lee con `fmt.Scanln` y
  bubbletea tiene stdin en modo raw: dos lectores se roban bytes.
- **Arranca sin credenciales**: si `initDownloader` falla, `runTUI` guarda el error en
  `bootErr` y abre el menú igual, porque el login vive ahí.
- Toda llamada bloqueante va en un `tea.Cmd`, nunca dentro de `Update`.
- El progreso llega por `p.Send()` desde el backend; `Update` reenvía al `Model` lo que no
  reconoce.
- Cada operación larga tiene su contexto cancelable: Ctrl+C cancela el trabajo y solo sale
  del programa si no hay nada corriendo.
- `lyrics.FetchAll` es la versión sin barras de `lyrics.Run`, para usarla bajo la TUI.
- `View()` pinta solo las filas que caben (`visibleTracks`). `viewChrome`/`shellChrome` son
  las filas fijas: **si cambias cabecera o pie, cámbialos** (`TestViewFitsTheScreen`).

### Idioma de la TUI

- El inglés vive en el código; `T()` traduce al renderizar y `lang.go` solo tiene el mapa
  `es`. Una clave que falta sale en inglés, nunca en blanco.
- `SetLang` se llama una vez desde `main()`, antes de cualquier `tea.Program` (no lleva lock).
- Los mensajes de estado van por `T()`. Los **errores** se quedan en inglés, sin `T()`, como
  en el resto de paquetes.
- Tests: `TestMenuRendersFullySpanish` compara la salida real (una tabla completa no detecta
  un sitio de render que no la consulta). `TestEnglishRenderStaysEnglish` renderiza en inglés
  y falla si aparece algún valor del mapa `es`, que es lo único que caza español hardcodeado
  sin acentos. `readUISources` lee **directorios**, no una lista de ficheros.

### CLI

`main()` solo registra flags, atiende los atajos de config (`--version`, `--reset`,
`--show-config`, `--purge`), monta el contexto cancelable y despacha. Nada de lógica inline.

- Un subcomando nuevo = `run<Name>(ctx, args, ...)` + una línea en el switch. Usa
  `requireArgs` y `mustDownloader`. Va en su propio `<name>_cmd.go` solo si tiene sustancia
  (~80 líneas).
- **Un solo `FlagSet` y `parseArgs`**. `flag` de stdlib para en el primer posicional, así
  que `dl <URL> -q 27` ignoraba `-q` en silencio. `parseArgs` quita un posicional cada vez y
  vuelve a parsear; el `--` literal se separa antes del bucle.
- `TestAdvertisedFlagsExist` comprueba que cada `qobuz-dl --xxx` citado en las fuentes esté
  registrado. Es estático a propósito: ejecutar `--reset` escribiría el config del usuario.

### Formatos de nombre: validar en la entrada, nunca degradar en silencio

`folder_format` y `track_format` son entrada del usuario, y aceptarlas mal no da error: da
pistas que no existen. Pasó en el issue #23:

1. Un placeholder desconocido sobrevivía tal cual en la plantilla.
2. Los 12 tracks del álbum resolvían al mismo nombre.
3. Los 3 workers renombraban al mismo destino; ganaba el último.
4. El resto veía el fichero presente, se saltaba sin aviso y quedaba en la DB.

Síntoma: lista 12, descarga 1, ningún error. Por eso:

- **Placeholder desconocido = error** que nombra el token y la lista válida.
- **Un `track_format` sin `{tracknumber}` ni `{tracktitle}` también es error.**
- `validateFormats` se llama desde `New`, el único punto común de CLI, TUI, csv y fun. La
  calidad se valida en `initDownloader`, antes de tocar la red.
- **Si `cleanFormatStr` sustituye la plantilla** (MP3 no tiene bit depth), lo avisa.
- **El límite de nombre es por componente y en bytes** (255). `limitNameBytes` recorta solo
  el último componente, por runas enteras, y añade el id del track para que dos títulos
  largos no choquen.
- `TestREADMEPlaceholderParity`: los placeholders del README y los de `expandPlaceholders`
  deben coincidir en los dos sentidos. El README llegó a anunciar `{genre}` y `{composer}`
  sin que existieran.

### Directorio de descarga

Prioridad: flag `-d` > `download_dir` en `config.ini` > `./qobuz-downloader`.
`config.ResolveDir(dir, create)` expande `~` y resuelve la ruta; `lyrics` lo llama con
`create=false` (la biblioteca debe existir). No confundir `Config.DownloadDir` (la ruta) con
la constante `config.DefaultFolder` (el formato de carpeta por defecto).

**El directorio no es nuestro**: `-d .` es la forma documentada de descargar en el CWD, así
que puede ser `~` o `/`. Nunca recorrer y borrar desde ahí; se borra solo lo que el run
escribió, por su ruta (`TestIntegration_DownloadURLsLeavesForeignFilesAlone`).

### Descargas: reintentos y stall timeout

`downloadWithProgress` reintenta hasta 5 veces con backoff (1/2/4/8 s) y reanuda con
`Range: bytes=N-`. Si el servidor ignora el Range (responde 200), trunca y empieza de cero.

**No hay tope total por petición.** Un `http.Client.Timeout` de 10 min hacía fallar toda
pista que tardara más, fuera cual fuera la velocidad. `newDownloadClient` pone un deadline de
lectura que avanza con cada `Read` (`stallTimeout`, 60 s): corta una conexión muerta, nunca
una lenta. Si el usuario paró se decide con `ctx.Err()`, no por el tipo del error.

### Autenticación y config

La autenticación por contraseña está rota (401 desde abril de 2026). Dos vías:
1. **OAuth** (`qobuz-dl oauth`): un servidor local captura el redirect con
   `user_auth_token=` o `code_autorisation=` (con `code` como fallback). Funciona end to end.
2. **Token** (`qobuz-dl --reset`): pegar user_id + user_auth_token desde DevTools.

`loadOrInitConfig(skipCredentials)` en `flags.go`: si hay `config.ini` lo carga; si no, con
`false` llama a `config.Reset()` (pide credenciales) y con `true` a `config.InitConfig()`
(solo preferencias). `initDownloader` pasa `false`; `runOAuth` pasa `true` y guarda el token
con `config.SaveToken`. `runLyrics` usa `config.Load()` directamente.

**Nunca pedir user_id/token en `oauth` ni en `lyrics`.**

**Cuando la auth falla, el consejo impreso es la única salida del usuario: tiene que ser el
correcto.** Ejemplos que ya fallaron: se anunciaba un `--token` que no existía, y una cuenta
gratuita (`IneligibleError`) recibía "usa `--reset`", que no arregla nada porque las
credenciales son correctas. La ruta de token propaga el error tal cual, sin consejo.

### `lyrics`

- LRCLIB: `GET /api/get?track_name&artist_name&album_name&duration`. Prefiere `syncedLyrics`
  sobre `plainLyrics`; 404 = sin letra (no es un error); 429 = un reintento tras `retryDelay`;
  `duration` se omite si es 0. 500 ms entre peticiones (`StepDelay`).
- Duración MP3: `TLEN` → cabecera Xing/Info → estimación CBR.
- `Run` → `runWithClient(dir, client)`: el cliente se inyecta por parámetro para los tests.
- Salta ficheros que ya tienen `.lrc`.

## Tests

### Reglas

- Stdlib solamente, offline, table-driven, subtests con `t.Run`.
- Inyección por parámetro (`Run` → `runWithClient`), no con clientes globales.
- Los fakes (`fakeFLAC`, `fakeMP3`) viven en `*_test.go`.
- Antes de añadir un test: busca si ya hay uno y extiende su tabla.
- `cmd/qobuz-dl` marca 0 % de cobertura porque sus tests son black-box: compilan el binario
  y lo ejecutan como subproceso.

### Técnicas que usamos

- **Validar un test con mutaciones**: rompe la línea a propósito y comprueba que el test
  falla. Un test que sigue verde con el código roto no prueba nada.
- **Aserción sobre el efecto propio.** Ejemplo: que la DB salte un track no se prueba viendo
  que no se re-descarga (el `os.Stat` de `downloadAndTag` ya lo impide), sino viendo que no
  se llama a `track/getFileUrl`.
- **Test diferencial desechable** para refactors con resultado silenciosamente rompible:
  copia la versión vieja como `xxxOld` en un `zz_diff_test.go`, compara con entradas
  aleatorias y **bórralo antes de commitear**.
- **Integración** (`integration_test.go`): servidor Qobuz falso + `rewriteTransport`,
  inyectado con `api.NewWithHTTP(appID, secrets, hc)` (el `baseURL` de `api` es const).

### Trampas ya pisadas

- **Codificaciones: usa siempre un carácter no ASCII.** `string(b)` reinterpreta los bytes
  como UTF-8, no convierte desde Latin-1: `Café` salía como `"Caf\xe9"`. Lo correcto es
  `rune(b)` byte a byte. Los tests en ASCII lo ocultaron.
- **Alineaciones: recorre todos los desplazamientos.** Un único dato puede caer en frontera
  por casualidad (120 runas de 3 bytes recortadas a 246 bytes, múltiplo de 3).
- **Una tabla completa no cubre un sitio de render que no la consulta.** Compara salida real.
- **`-race` en local.** CI corre `go test -race` y estuvo rojo cuatro pushes seguidos sin que
  nadie lo viera: `stallConn.Read` leía la global `stallTimeout` desde la goroutine de
  `net/http`, que sobrevive al subtest, mientras el `t.Cleanup` la restauraba. Una global que
  un test ajusta no debe leerse desde goroutines que no son del test; captúrala al construir
  el objeto. Tras cada push, `gh run list`.
- **La documentación también se testea**: `TestAdvertisedFlagsExist` y
  `TestREADMEPlaceholderParity` existen porque la doc anunció cosas que no existían.

### CI (`.github/workflows/ci.yml`)

`gofmt -l .` vacío → `go vet ./...` → `go test -race -cover ./...`.

**`go 1.26` va sin parche a propósito.** `setup-go` (CI y release) lee esa línea: con un
parche fijo instala exactamente ese (así se estuvo compilando con 1.24.0, con 32
vulnerabilidades de stdlib); sin parche instala la última 1.26.x. La línea `toolchain`
es para local: sin ella `GOTOOLCHAIN=auto` busca un `go1.26` que no existe.

## Medido y descartado

Para no volver a investigarlo:

- El buffer de 32 KB de `io.Copy` por intento y los cierres de `mpb` por `Read`: basura
  (~1 MB por álbum), no RSS.
- El RSS de `--version` (8,4 MB) es casi todo páginas del binario. Para separar memoria
  propia de compartida usa `RssAnon`/`RssFile` en `/proc/<pid>/status`, no `/usr/bin/time`.
- "LRCLIB no reutiliza la conexión tras un 404": falso. Habla HTTP/2 y la conexión se
  reutiliza (visto con `httptrace`).
- Complejidad cognitiva: la penaliza el **anidamiento**, no el número de pasadas. Colapsar
  pasadas no la bajó; sacar el trabajo interno a una función sí. Mayor valor hoy: `pickBest`
  (14).
- **Last.fm retirado** (2026-09-23): la API XSPF 1.0 devolvía 404 siempre. Si vuelve, que sea
  con la API 2.0 y una `api_key` en `config.ini`.
