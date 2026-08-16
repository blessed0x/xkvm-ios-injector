package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/decrypt"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// DecryptOptions drives xkvm decrypt. AppID accepts a numeric App Store id,
// an apps.apple.com URL, or a bundle id (looked up). Credentials are optional
// when a session was saved by an earlier login.
type DecryptOptions struct {
	AppleID     string
	Password    string
	AppID       string
	Version     string // external version id; empty = latest
	OutputDir   string
	Interactive bool // allow credential prompts when stdin is a terminal
}

// RunDecrypt downloads an App Store app (the ipatool / PancakeStore flow)
// and writes a re-processed IPA — iTunesMetadata.plist + SC_Info sinfs — to
// the output directory. Returns the written .ipa path.
func RunDecrypt(ctx context.Context, opts DecryptOptions) (string, error) {
	client, err := decryptClient(ctx, opts)
	if err != nil {
		return "", err
	}

	appID, err := decrypt.ResolveAppID(ctx, opts.AppID)
	if err != nil {
		return "", err
	}

	outDir := opts.OutputDir
	if outDir == "" {
		if p, _ := decrypt.LoadPrefs(); p.OutputDir != "" {
			outDir = p.OutputDir
		}
	}
	if outDir == "" {
		outDir = "."
	}

	log.Infof("downloading app %d...", appID)
	path, err := client.Download(ctx, appID, opts.Version, outDir)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	log.Infof("wrote %s", path)
	return path, nil
}

// decryptClient restores a saved session, or authenticates when credentials
// are supplied (prompting for them first in interactive mode).
func decryptClient(ctx context.Context, opts DecryptOptions) (*decrypt.Client, error) {
	appleID, password := opts.AppleID, opts.Password
	if appleID == "" && opts.Interactive {
		appleID, password = promptCredentials()
	}
	if appleID != "" {
		if password == "" {
			return nil, fmt.Errorf("--apple-id given without --password")
		}
		client := decrypt.New(appleID, password)
		if err := client.Authenticate(ctx); err != nil {
			return nil, err
		}
		if err := client.Save(); err != nil {
			log.Warnf("couldn't save the session: %v", err)
		}
		log.Infof("signed in as %s", client.AccountName)
		return client, nil
	}

	client := decrypt.New("", "")
	if err := client.Load(); err != nil {
		return nil, err
	}
	if !client.HasAuth() {
		return nil, errors.New("not signed in: run again with --apple-id and --password, or log in through the menu")
	}
	log.Infof("using saved session for %s", client.AppleID)
	return client, nil
}

// promptCredentials reads an Apple ID and password from stdin. The password
// is echoed (no hidden-input support without a term package); users on
// shared screens should pass --password instead.
func promptCredentials() (string, string) {
	r := bufio.NewReader(os.Stdin)
	fmt.Fprint(os.Stdout, "Apple ID: ")
	id, _ := r.ReadString('\n')
	fmt.Fprint(os.Stdout, "Password: ")
	pw, _ := r.ReadString('\n')
	return strings.TrimSpace(id), strings.TrimSpace(pw)
}
