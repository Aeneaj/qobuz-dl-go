package main

import (
	"context"
	"fmt"
	"os"

	"github.com/Aeneaj/qobuz-dl-go/internal/api"
	"github.com/Aeneaj/qobuz-dl-go/internal/bundle"
	"github.com/Aeneaj/qobuz-dl-go/internal/config"
	"github.com/Aeneaj/qobuz-dl-go/internal/downloader"
)

func runOAuth(ctx context.Context, codeOrURL string) {
	cfg, err := loadOrInitConfig(true)
	if err != nil {
		fatalf("load config: %v", err)
	}

	appID := cfg.AppID
	privateKey := cfg.PrivateKey
	secrets := cfg.Secrets

	// Always fetch a fresh bundle for OAuth so that app_id, secrets and
	// private_key are current (they rotate). Also recovers from configs
	// written when private_key regex didn't match.
	fmt.Println("\033[33mFetching app tokens from Qobuz bundle.js...\033[0m")
	b, bundleErr := bundle.Fetch(ctx)
	if bundleErr != nil {
		fmt.Printf("\033[33mWarning: bundle fetch failed (%v) — falling back to cached config values\033[0m\n", bundleErr)
	} else {
		if id, err := b.AppID(); err == nil && id != "" {
			appID = id
		}
		if pk := b.PrivateKey(); pk != "" {
			privateKey = pk
		}
		if sec, err := b.Secrets(); err == nil {
			var fresh []string
			for _, v := range sec {
				if v != "" {
					fresh = append(fresh, v)
				}
			}
			if len(fresh) > 0 {
				secrets = fresh
			}
		}

		// Persist refreshed bundle values so future runs don't need to re-fetch
		if err := config.UpdateBundleKeys(cfg.FilePath, appID, privateKey, secrets); err != nil {
			fmt.Printf("\033[33mWarning: could not update config with fresh bundle keys: %v\033[0m\n", err)
		}
	}

	if appID == "" {
		fatalf("app_id not found — run 'qobuz-dl --reset' to reconfigure")
	}
	if privateKey == "" {
		fmt.Println("\033[31mWarning: private_key not found in bundle.js.\033[0m")
		fmt.Println("\033[33mOAuth code exchange will fail. If it does, use token auth:\033[0m")
		fmt.Println("\033[33m  qobuz-dl --reset --token\033[0m")
	}

	client := api.New(appID, secrets)
	dl, err := downloader.New(client, downloader.Options{})
	if err != nil {
		fatalf("init downloader: %v", err)
	}

	if err := dl.OAuthLogin(ctx, appID, privateKey, codeOrURL); err != nil {
		fmt.Fprintf(os.Stderr, "\033[31m%v\033[0m\n", err)
		os.Exit(1)
	}

	// Persist token + user_id
	if client.UAT != "" {
		if err := config.SaveToken(cfg.FilePath, client.UserID, client.UAT); err != nil {
			fmt.Fprintf(os.Stderr, "\033[33mWarning: could not save token: %v\033[0m\n", err)
		}
	} else {
		fmt.Println("\033[33mWarning: no token obtained — config not updated\033[0m")
	}
}
