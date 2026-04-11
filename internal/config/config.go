// Package config handles qobuz-dl configuration (~/.config/qobuz-dl/config.ini).
// Uses only stdlib — no external dependencies.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Aeneaj/qobuz-dl-go/internal/bundle"
)

const (
	DefaultFolder = "{artist} - {album} ({year}) [{bit_depth}B-{sampling_rate}kHz]"
	DefaultTrack  = "{tracknumber}. {tracktitle}"
)

// Config holds all runtime settings.
type Config struct {
	UserID         string
	UserAuthToken  string
	DefaultFolder  string
	DefaultQuality int
	DefaultLimit   int
	NoM3U          bool
	AlbumsOnly     bool
	NoFallback     bool
	OGCover        bool
	EmbedArt       bool
	NoCover        bool
	NoDatabase     bool
	AppID          string
	Secrets        []string
	PrivateKey     string
	FolderFormat   string
	TrackFormat    string
	SmartDiscog    bool
	FilePath       string
	DBPath         string
}

// ConfigDir returns the OS config directory for qobuz-dl.
func ConfigDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "qobuz-dl")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "qobuz-dl")
}

// Load reads config.ini from disk.
func Load() (*Config, error) {
	dir := ConfigDir()
	cfgFile := filepath.Join(dir, "config.ini")

	ini, err := readINI(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	get := func(key, def string) string {
		if v, ok := ini[key]; ok && v != "" {
			return v
		}
		return def
	}
	getBool := func(key string) bool {
		v := strings.ToLower(strings.TrimSpace(ini[key]))
		return v == "true" || v == "1" || v == "yes"
	}
	getInt := func(key string, def int) int {
		if v, ok := ini[key]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
		return def
	}

	var secrets []string
	for _, s := range strings.Split(get("secrets", ""), ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			secrets = append(secrets, s)
		}
	}

	return &Config{
		UserID:         get("user_id", ""),
		UserAuthToken:  get("user_auth_token", ""),
		DefaultFolder:  get("default_folder", "Qobuz Downloads"),
		DefaultQuality: getInt("default_quality", 6),
		DefaultLimit:   getInt("default_limit", 20),
		NoM3U:          getBool("no_m3u"),
		AlbumsOnly:     getBool("albums_only"),
		NoFallback:     getBool("no_fallback"),
		OGCover:        getBool("og_cover"),
		EmbedArt:       getBool("embed_art"),
		NoCover:        getBool("no_cover"),
		NoDatabase:     getBool("no_database"),
		AppID:          get("app_id", ""),
		Secrets:        secrets,
		PrivateKey:     get("private_key", ""),
		FolderFormat:   get("folder_format", DefaultFolder),
		TrackFormat:    get("track_format", DefaultTrack),
		SmartDiscog:    getBool("smart_discography"),
		FilePath:       cfgFile,
		DBPath:         filepath.Join(dir, "qobuz_dl.db"),
	}, nil
}

// Reset interactively creates a fresh config.ini.
// Credentials are set via OAuth token (user_id + user_auth_token).
func Reset() error {
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	cfgFile := filepath.Join(dir, "config.ini")
	fmt.Printf("\033[33mCreating config file: %s\033[0m\n", cfgFile)

	kv := map[string]string{}

	fmt.Println("\033[33mTo get your credentials: log in at https://play.qobuz.com,")
	fmt.Println("open DevTools → Application → Local Storage → find 'localuser'\033[0m")
	kv["user_id"] = prompt("Enter your Qobuz user_id:\n- ")
	kv["user_auth_token"] = prompt("Enter your Qobuz user_auth_token:\n- ")

	kv["default_folder"] = promptDefault("Folder for downloads (leave empty for 'Qobuz Downloads')\n- ", "Qobuz Downloads")
	kv["default_quality"] = promptDefault("Download quality (5, 6, 7, 27) [320, LOSSLESS, 24B <96KHZ, 24B >96KHZ]\n(leave empty for '6')\n- ", "6")
	kv["default_limit"] = "20"
	kv["no_m3u"] = "false"
	kv["albums_only"] = "false"
	kv["no_fallback"] = "false"
	kv["og_cover"] = "false"
	kv["embed_art"] = "false"
	kv["no_cover"] = "false"
	kv["no_database"] = "false"
	kv["smart_discography"] = "false"
	// Kept for backward compat reading old configs; never written by Reset
	kv["email"] = ""
	kv["password"] = ""
	kv["folder_format"] = DefaultFolder
	kv["track_format"] = DefaultTrack

	fmt.Println("\033[33mFetching app tokens from Qobuz web player, please wait...\033[0m")
	b, err := bundle.Fetch()
	if err != nil {
		return fmt.Errorf("fetch bundle: %w", err)
	}
	appID, err := b.AppID()
	if err != nil {
		return err
	}
	secrets, err := b.Secrets()
	if err != nil {
		return err
	}
	var secList []string
	for _, v := range secrets {
		if v != "" {
			secList = append(secList, v)
		}
	}
	kv["app_id"] = appID
	kv["secrets"] = strings.Join(secList, ",")
	kv["private_key"] = b.PrivateKey()

	if err := writeINI(cfgFile, kv); err != nil {
		return err
	}
	fmt.Printf("\033[32mConfig saved to %s\033[0m\n", cfgFile)
	return nil
}

// UpdateBundleKeys patches app_id, secrets and private_key in config.ini without
// touching credentials or user preferences. Called after a fresh bundle fetch.
func UpdateBundleKeys(cfgFile, appID, privateKey string, secrets []string) error {
	ini, err := readINI(cfgFile)
	if err != nil {
		ini = map[string]string{}
	}
	if appID != "" {
		ini["app_id"] = appID
	}
	if privateKey != "" {
		ini["private_key"] = privateKey
	}
	if len(secrets) > 0 {
		ini["secrets"] = strings.Join(secrets, ",")
	}
	return writeINI(cfgFile, ini)
}

// SaveToken updates user_id and user_auth_token in config.ini after OAuth login.
func SaveToken(cfgFile, userID, uat string) error {
	ini, err := readINI(cfgFile)
	if err != nil {
		ini = map[string]string{}
	}
	ini["user_auth_token"] = uat
	if userID != "" {
		ini["user_id"] = userID
	}
	if err := writeINI(cfgFile, ini); err != nil {
		return err
	}
	fmt.Println("\033[32mOAuth token saved to config.ini\033[0m")
	return nil
}

// ---- minimal INI reader/writer ----

func readINI(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		val := strings.TrimSpace(line[idx+1:])
		m[key] = val
	}
	return m, scanner.Err()
}

func writeINI(path string, kv map[string]string) error {
	order := []string{
		"user_id", "user_auth_token",
		"email", "password", // legacy — kept for reading old configs
		"default_folder", "default_quality", "default_limit",
		"no_m3u", "albums_only", "no_fallback", "og_cover",
		"embed_art", "no_cover", "no_database", "smart_discography",
		"app_id", "secrets", "private_key",
		"folder_format", "track_format",
	}
	written := map[string]bool{}
	var sb strings.Builder
	sb.WriteString("[DEFAULT]\n")
	for _, k := range order {
		if v, ok := kv[k]; ok {
			fmt.Fprintf(&sb, "%s = %s\n", k, v)
			written[k] = true
		}
	}
	for k, v := range kv {
		if !written[k] {
			fmt.Fprintf(&sb, "%s = %s\n", k, v)
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0600)
}

// ---- prompt helpers ----

func prompt(msg string) string {
	fmt.Print(msg)
	r := bufio.NewReader(os.Stdin)
	s, _ := r.ReadString('\n')
	return strings.TrimSpace(s)
}

func promptDefault(msg, def string) string {
	v := prompt(msg)
	if v == "" {
		return def
	}
	return v
}
