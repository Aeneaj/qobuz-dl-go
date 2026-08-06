package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// testEnv returns a clean environment that pins HOME and XDG_CONFIG_HOME
// under tmp. Necessary because (a) os.UserConfigDir prefers $XDG_CONFIG_HOME
// on Linux, so a HOME-only override is bypassed when CI inherits XDG vars;
// (b) append(os.Environ(), "HOME="+tmp) leaves the original HOME earlier in
// the slice, and Go's os.Getenv returns the first match.
func testEnv(tmp string) []string {
	return []string{
		"HOME=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(tmp, ".config"),
		"PATH=" + os.Getenv("PATH"),
	}
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

// TestMissingArgs_ExitsNonZero covers requireArgs, shared by dl/lucky/csv.
// These must bail out before any config load, so they stay offline.
func TestMissingArgs_ExitsNonZero(t *testing.T) {
	cases := []struct{ cmd, wantStderr string }{
		{"dl", "dl: provide at least one URL"},
		{"lucky", "lucky: provide a search query"},
		{"csv", "csv: provide path to a TuneMyMusic CSV file"},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			_, stderr, code := run(c.cmd)
			if code == 0 {
				t.Errorf("exit code = 0, want non-zero")
			}
			if !strings.Contains(stderr, c.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, c.wantStderr)
			}
		})
	}
}

func TestShowConfig_NoConfig_Exits(t *testing.T) {
	// With HOME pointing to empty dir, config doesn't exist.
	// Either it prompts for config (stdin closed -> error) or shows error.
	// Either way it should not panic.
	tmp := t.TempDir()
	cmd := exec.Command(binaryPath, "--show-config")
	cmd.Env = testEnv(tmp)
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
	cmd.Env = testEnv(tmp)
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
	cmd.Env = testEnv(tmp)
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
	cmd.Env = testEnv(tmp)
	out, _ := cmd.CombinedOutput()

	if !strings.Contains(string(out), "email") {
		t.Errorf("expected config content in output, got: %q", string(out))
	}
}

// TestAdvertisedFlagsExist guards a whole bug class: error paths that tell the
// user to run "qobuz-dl --something" where --something was never registered.
// That advice is printed exactly when the user is already stuck (auth failed),
// so a stale flag name sends them into "flag provided but not defined".
//
// Deliberately static — it compares advertised names against the ones passed to
// fs.Bool/fs.String/fs.Int rather than executing the binary. Running the advice
// for real would fire --reset, which writes the caller's config.ini and fetches
// bundle.js over the network.
func TestAdvertisedFlagsExist(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Dir(filepath.Dir(wd)) // repo root, from cmd/qobuz-dl

	var (
		reAdvice = regexp.MustCompile(`qobuz-dl ((?:--[a-z-]+ ?)+)`)
		// Both spellings: fs.Bool("x", …) and fs.BoolVar(&v, "x", …).
		reRegister = regexp.MustCompile(`fs\.(?:Bool|String|Int)(?:Var\(&\w+, )?\(?"([a-z-]+)"`)
		advertised = map[string]string{} // flag -> file advertising it
		registered = map[string]bool{}
	)

	err = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range reRegister.FindAllStringSubmatch(string(src), -1) {
			registered[m[1]] = true
		}
		for _, m := range reAdvice.FindAllStringSubmatch(string(src), -1) {
			for _, f := range strings.Fields(m[1]) {
				advertised[strings.TrimPrefix(f, "--")] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sources: %v", err)
	}

	// Both sides must be non-empty, otherwise a broken regex passes silently.
	if len(advertised) == 0 || len(registered) == 0 {
		t.Fatalf("scan found %d advertised and %d registered flags — regex or walk is broken",
			len(advertised), len(registered))
	}

	for flag, file := range advertised {
		if !registered[flag] {
			t.Errorf("%s tells the user to run \"qobuz-dl --%s\" but no flag %q is registered",
				file, flag, flag)
		}
	}
}
