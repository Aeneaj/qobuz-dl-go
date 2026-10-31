package main

import (
	"context"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/bundle"
	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
)

func runOAuth(ctx context.Context, args []string) {
	codeOrURL := ""
	if len(args) > 0 {
		codeOrURL = args[0]
	}
	if err := oauthLogin(ctx, codeOrURL, nil); err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m%v\033[0m\n", err)
		os.Exit(1)
	}
}

// oauthLogin runs the whole OAuth flow and saves the token. It prints to the
// terminal directly, so a TUI caller must release the terminal first and pass
// its program: without one the flow also reads Enter from stdin.
func oauthLogin(ctx context.Context, codeOrURL string, prog *tea.Program) error {
	cfg, err := loadOrInitConfig(ctx, true)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	appID := cfg.AppID
	privateKey := cfg.PrivateKey
	secrets := cfg.Secrets

	// Always fetch a fresh bundle for OAuth so that app_id, secrets and
	// private_key are current (they rotate). Also recovers from configs
	// written when private_key regex didn't match.
	fmt.Println("\033[33mFetching app tokens from Qobuz bundle.js (can take up to 30 seconds)...\033[0m")
	b, bundleErr := bundle.Fetch(ctx)
	if ctx.Err() != nil { // Ctrl+C is not a network failure to fall back from
		return ctx.Err()
	}
	if bundleErr != nil {
		fmt.Printf("\033[33mWarning: bundle fetch failed (%v) — falling back to cached config values\033[0m\n", bundleErr)
	} else {
		if id, err := b.AppID(); err == nil && id != "" {
			appID = id
		}
		if pk := b.PrivateKey(); pk != "" {
			privateKey = pk
		}
		if sec, err := b.Secrets(); err == nil && len(sec) > 0 {
			secrets = sec
		}

		// Persist refreshed bundle values so future runs don't need to re-fetch
		if err := config.UpdateBundleKeys(cfg.FilePath, appID, privateKey, secrets); err != nil {
			fmt.Printf("\033[33mWarning: could not update config with fresh bundle keys: %v\033[0m\n", err)
		}
	}

	if appID == "" {
		return fmt.Errorf("app_id not found — run 'qobuz-dl --reset' to reconfigure")
	}
	if privateKey == "" {
		fmt.Println("\033[31mWarning: private_key not found in bundle.js.\033[0m")
		fmt.Println("\033[33mOAuth code exchange will fail. If it does, use token auth:\033[0m")
		fmt.Println("\033[33m  qobuz-dl --reset\033[0m")
	}

	client := api.New(appID, secrets)
	dl, err := downloader.New(client, downloader.Options{})
	if err != nil {
		return fmt.Errorf("init downloader: %w", err)
	}
	dl.SetUI(prog)

	if err := dl.OAuthLogin(ctx, appID, privateKey, codeOrURL); err != nil {
		return err
	}

	// Persist token + user_id
	if client.UAT == "" {
		return fmt.Errorf("no token obtained — config not updated")
	}
	if err := config.SaveToken(cfg.FilePath, client.UserID, client.UAT); err != nil {
		return fmt.Errorf("could not save token: %w", err)
	}
	return nil
}
