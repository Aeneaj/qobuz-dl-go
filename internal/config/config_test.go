package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadINI_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")

	kv := map[string]string{
		"email":           "user@example.com",
		"default_quality": "6",
		"no_m3u":          "false",
		"secrets":         "sec1,sec2,sec3",
		"folder_format":   "{artist} - {album}",
	}
	if err := writeINI(path, kv); err != nil {
		t.Fatalf("writeINI: %v", err)
	}

	got, err := readINI(path)
	if err != nil {
		t.Fatalf("readINI: %v", err)
	}

	for k, want := range kv {
		if got[k] != want {
			t.Errorf("key %q: got %q, want %q", k, got[k], want)
		}
	}
}

func TestReadINI_IgnoresComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	content := "[DEFAULT]\n# this is a comment\n; also a comment\nemail = test@test.com\n"
	os.WriteFile(path, []byte(content), 0644)

	got, err := readINI(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["email"] != "test@test.com" {
		t.Errorf("email = %q", got["email"])
	}
	if _, ok := got["# this is a comment"]; ok {
		t.Error("comment was parsed as key")
	}
}

func TestReadINI_CaseInsensitiveKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	os.WriteFile(path, []byte("[DEFAULT]\nEMAIL = upper@case.com\n"), 0644)
	got, _ := readINI(path)
	if got["email"] != "upper@case.com" {
		t.Errorf("expected lowercase key, got %v", got)
	}
}

func TestLoad_ParsesSecretsCSV(t *testing.T) {
	dir := t.TempDir()

	// Override ConfigDir by setting HOME
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", origHome)

	cfgDir := filepath.Join(dir, ".config", "qobuz-dl")
	os.MkdirAll(cfgDir, 0755)

	content := "[DEFAULT]\n" +
		"email = \npassword = \nuser_id = \nuser_auth_token = \n" +
		"default_folder = Qobuz Downloads\ndefault_quality = 6\n" +
		"default_limit = 20\nno_m3u = false\nalbums_only = false\n" +
		"no_fallback = false\nog_cover = false\nembed_art = false\n" +
		"no_cover = false\nno_database = false\nsmart_discography = false\n" +
		"app_id = 123456789\nsecrets = secret1,secret2,secret3\n" +
		"private_key = mykey\n" +
		"folder_format = {artist} - {album}\ntrack_format = {tracknumber}. {tracktitle}\n"
	os.WriteFile(filepath.Join(cfgDir, "config.ini"), []byte(content), 0644)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Secrets) != 3 {
		t.Errorf("expected 3 secrets, got %d: %v", len(cfg.Secrets), cfg.Secrets)
	}
	if cfg.Secrets[0] != "secret1" {
		t.Errorf("first secret = %q", cfg.Secrets[0])
	}
	if cfg.AppID != "123456789" {
		t.Errorf("AppID = %q", cfg.AppID)
	}
	if cfg.DefaultQuality != 6 {
		t.Errorf("DefaultQuality = %d", cfg.DefaultQuality)
	}
}

func TestSaveToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	initial := map[string]string{
		"user_id":         "",
		"user_auth_token": "",
		"email":           "test@test.com",
	}
	writeINI(path, initial)

	if err := SaveToken(path, "777", "newtoken123"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, _ := readINI(path)
	if got["user_auth_token"] != "newtoken123" {
		t.Errorf("user_auth_token = %q", got["user_auth_token"])
	}
	if got["user_id"] != "777" {
		t.Errorf("user_id = %q", got["user_id"])
	}
	// Original values preserved
	if got["email"] != "test@test.com" {
		t.Errorf("email was lost: %q", got["email"])
	}
}

func TestWriteINI_StableKeyOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	kv := map[string]string{
		"email":    "a@b.com",
		"password": "hash",
		"app_id":   "123",
		"secrets":  "s1,s2",
	}
	writeINI(path, kv)

	data, _ := os.ReadFile(path)
	content := string(data)
	// email should appear before app_id (per ordered list)
	emailPos := strings.Index(content, "email")
	appIDPos := strings.Index(content, "app_id")
	if emailPos > appIDPos {
		t.Errorf("expected email before app_id in output")
	}
}
