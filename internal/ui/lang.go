package ui

import "strings"

// The TUI ships in English and translates out of it: every string is written in
// English at its use site and looked up here at render time. A missing entry
// falls back to the key, so an untranslated string shows in English instead of
// a blank — there is no table of English strings to keep in sync, and adding a
// language cannot leave holes in the screen.
//
// SetLang is called once at startup, before the program runs, so no locking is
// needed. Do not call it from a running TUI.
var translations map[string]string

// SetLang selects the language by two-letter code. Anything unknown (including
// "" and "en") leaves the UI in English.
func SetLang(code string) {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "es":
		translations = es
	default:
		translations = nil
	}
}

// T translates s into the active language, or returns s unchanged.
func T(s string) string {
	if t, ok := translations[s]; ok {
		return t
	}
	return s
}

var es = map[string]string{
	// menu sections
	"FIND":    "BUSCAR",
	"QUEUE":   "COLA",
	"TOOLS":   "HERRAMIENTAS",
	"SESSION": "SESIÓN",

	// menu labels
	"Search albums":      "Buscar álbumes",
	"Search tracks":      "Buscar canciones",
	"Search artists":     "Buscar artistas",
	"Search playlists":   "Buscar playlists",
	"Add URL":            "Añadir URL",
	"View the queue":     "Ver la cola",
	"Download the queue": "Descargar la cola",
	"Lyrics (.lrc)":      "Letras (.lrc)",
	"Import CSV":         "Importar CSV",
	"Settings":           "Configuración",
	"Clear history":      "Borrar historial",
	"Log in (OAuth)":     "Iniciar sesión (OAuth)",
	"Quit":               "Salir",

	// menu hints
	"search Qobuz and add to the queue":      "busca en Qobuz y añade a la cola",
	"search by track":                        "búsqueda por track",
	"full discography":                       "discografía completa",
	"Qobuz playlists":                        "playlists de Qobuz",
	"album, track, artist, label or Last.fm": "álbum, track, artista, sello o Last.fm",
	"review and remove items":                "revisar y quitar elementos",
	"start downloading":                      "empieza la descarga",
	"find synced lyrics on LRCLIB":           "busca letras sincronizadas en LRCLIB",
	"playlist exported from TuneMyMusic":     "playlist exportada de TuneMyMusic",
	"show current settings":                  "ver ajustes actuales",
	"forget what was already downloaded":     "olvida lo ya descargado",
	"opens Qobuz in your browser":            "abre Qobuz en el navegador",

	// input prompts
	"Search ":               "Buscar ",
	"Qobuz or Last.fm URL:": "URL de Qobuz o Last.fm:",
	"Folder to scan:":       "Carpeta que escanear:",
	"Path to the CSV:":      "Ruta del CSV:",

	// status lines
	"space marks · enter adds to the queue · esc goes back": "espacio marca · enter añade a la cola · esc vuelve",
	"d removes · enter downloads · esc goes back":           "d quita · enter descarga · esc vuelve",
	"  ·  enter to return to the menu":                      "  ·  enter para volver al menú",
	"cancelling…":                                           "cancelando…",
	"searching…":                                            "buscando…",
	"working…":                                              "trabajando…",
	"download finished":                                     "descarga terminada",
	"download history cleared":                              "historial de descargas borrado",
	"the queue is empty":                                    "la cola está vacía",
	"%d in the queue":                                       "%d en la cola",

	// header and footer
	"not signed in": "sin sesión",
	"signed in":     "conectado",
	"queue ":        "cola ",
	"move":          "moverse",
	"choose":        "elegir",
	"back":          "volver",
	"quit":          "salir",

	// download screen badges and footer
	"BATCH":            "LOTE",
	"DONE":             "LISTO",
	"%d done":          "%d completadas",
	"%d failed":        "%d errores",
	"%d active":        "%d activas",
	"Ctrl+C to cancel": "Ctrl+C cancelar",

	// picker
	"  (empty)": "  (vacío)",

	// backend status messages (cmd/qobuz-dl/tui_cmd.go). Errors from the
	// backend stay in English, like every other error in the program.
	"signed in successfully":        "sesión iniciada",
	"CSV import finished":           "importación CSV terminada",
	"scanning %s…":                  "escaneando %s…",
	"%d audio files found":          "%d archivos de audio encontrados",
	"no audio found in that folder": "no se encontró audio en esa carpeta",
	"lyrics: %d new · %d already there · %d without a match": "letras: %d nuevas · %d ya estaban · %d sin resultado",
	"could not read the settings":                            "no se pudo leer la configuración",
}
