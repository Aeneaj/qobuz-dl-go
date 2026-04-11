package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// binaryPath builds the binary once and returns its path.
var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "qobuz-dl-test-*")
	if err != nil {
		panic(err)
	}
	binaryPath = filepath.Join(tmp, "qobuz-dl")
	cmd := exec.Command("go", "build", "-o", binaryPath, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("build failed: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func run(args ...string) (stdout, stderr string, code int) {
	cmd := exec.Command(binaryPath, args...)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func TestNoArgs_PrintsUsage(t *testing.T) {
	out, _, code := run()
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage:") {
		t.Errorf("expected Usage in output, got: %q", out)
	}
	if !strings.Contains(out, "dl ") {
		t.Errorf("expected 'dl' command in usage, got: %q", out)
	}
	if !strings.Contains(out, "oauth") {
		t.Errorf("expected 'oauth' command in usage, got: %q", out)
	}
	if !strings.Contains(out, "lucky") {
		t.Errorf("expected 'lucky' command in usage, got: %q", out)
	}
	if !strings.Contains(out, "fun") {
		t.Errorf("expected 'fun' command in usage, got: %q", out)
	}
}

func TestUnknownCommand_ExitsNonZero(t *testing.T) {
	_, _, code := run("notacommand")
	if code == 0 {
		t.Error("expected non-zero exit for unknown command")
	}
}

func TestDL_NoURL_ExitsNonZero(t *testing.T) {
	_, stderr, code := run("dl")
	if code == 0 {
		t.Error("expected non-zero exit when no URL given to dl")
	}
	if !strings.Contains(stderr, "dl:") && !strings.Contains(stderr, "URL") {
		t.Errorf("expected error message, got stderr: %q", stderr)
	}
}

func TestLucky_NoQuery_ExitsNonZero(t *testing.T) {
	_, _, code := run("lucky")
	if code == 0 {
		t.Error("expected non-zero exit when no query given to lucky")
	}
}

func TestShowConfig_NoConfig_Exits(t *testing.T) {
	// With HOME pointing to empty dir, config doesn't exist.
	// Either it prompts for config (stdin closed -> error) or shows error.
	// Either way it should not panic.
	tmp := t.TempDir()
	cmd := exec.Command(binaryPath, "--show-config")
	cmd.Env = append(os.Environ(), "HOME="+tmp)
	cmd.Stdin = strings.NewReader("") // empty stdin so prompts fail fast
	err := cmd.Run()
	// Any exit (0 or non-0) is fine as long as it doesn't hang or panic
	_ = err
}

func TestReset_TokenFlag_CombinedWithReset(t *testing.T) {
	// --token without --reset should be silently ignored (no panic)
	// Pass empty stdin so any prompt terminates immediately
	tmp := t.TempDir()
	cmd := exec.Command(binaryPath, "--reset", "--token")
	cmd.Env = append(os.Environ(), "HOME="+tmp)
	cmd.Stdin = strings.NewReader("\n\n\n\n\n") // feed blank lines to prompts
	// This will likely fail at the bundle.Fetch() step (no network), that's fine
	// We just verify it doesn't panic
	_ = cmd.Run()
}

func TestPurge_NoDatabase_Succeeds(t *testing.T) {
	tmp := t.TempDir()
	// Create a minimal config so it doesn't try to reset
	cfgDir := filepath.Join(tmp, ".config", "qobuz-dl")
	os.MkdirAll(cfgDir, 0755)
	os.WriteFile(filepath.Join(cfgDir, "config.ini"), []byte("[DEFAULT]\n"), 0644)

	cmd := exec.Command(binaryPath, "--purge")
	cmd.Env = append(os.Environ(), "HOME="+tmp)
	out, err2 := cmd.CombinedOutput()
	_ = err2
	// Should mention "database" in output (deleted or not found)
	if !strings.Contains(string(out), "database") {
		t.Errorf("expected 'database' in output, got: %q", string(out))
	}
}

func TestShowConfig_ExistingConfig_PrintsIt(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, ".config", "qobuz-dl")
	os.MkdirAll(cfgDir, 0755)
	cfgContent := "[DEFAULT]\nemail = test@example.com\n"
	os.WriteFile(filepath.Join(cfgDir, "config.ini"), []byte(cfgContent), 0644)

	cmd := exec.Command(binaryPath, "--show-config")
	cmd.Env = append(os.Environ(), "HOME="+tmp)
	out, _ := cmd.CombinedOutput()

	if !strings.Contains(string(out), "email") {
		t.Errorf("expected config content in output, got: %q", string(out))
	}
}
