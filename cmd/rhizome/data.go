package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// resolveDataDir is the pure data-directory policy (FR-RHZ-125). All inputs
// that vary by host are injected so every branch can be tested without using
// the operator's real home directory.
func resolveDataDir(flagValue string, lookup func(string) (string, bool), goos, home, cwd string) (string, error) {
	path := flagValue
	if path == "" && lookup != nil {
		path, _ = lookup("RHIZOME_DATA_DIR")
	}
	if path == "" {
		switch goos {
		case "darwin":
			if home == "" {
				return "", fmt.Errorf("home directory is unset; set -data-dir or HOME")
			}
			path = filepath.Join(home, "Library", "Application Support", "rhizome")
		case "windows":
			if lookup == nil {
				return "", fmt.Errorf("LocalAppData is unset; set -data-dir or LocalAppData")
			}
			path, _ = lookup("LOCALAPPDATA")
			if path == "" {
				path, _ = lookup("LocalAppData")
			}
			if path == "" {
				return "", fmt.Errorf("LocalAppData is unset; set -data-dir or LocalAppData")
			}
			path = filepath.Join(path, "rhizome")
		default:
			if lookup != nil {
				if xdg, ok := lookup("XDG_DATA_HOME"); ok && xdg != "" && filepath.IsAbs(xdg) {
					path = filepath.Join(xdg, "rhizome")
				}
			}
			if path == "" {
				if home == "" {
					return "", fmt.Errorf("home directory is unset; set -data-dir or HOME")
				}
				path = filepath.Join(home, ".local", "share", "rhizome")
			}
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), nil
}

func commandDataDir(flagValue string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	return resolveDataDir(flagValue, os.LookupEnv, runtime.GOOS, home, cwd)
}

func resolveJournal(dataDirFlag, journal string, journalSet bool) (path, dataDir string, defaulted bool, err error) {
	if journalSet {
		if journal == "" {
			return "", "", false, fmt.Errorf("journal is required")
		}
		return journal, "", false, nil
	}
	dataDir, err = commandDataDir(dataDirFlag)
	if err != nil {
		return "", "", false, err
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return "", "", false, err
	}
	return filepath.Join(dataDir, "journal.ndjson"), dataDir, true, nil
}

// resolveExistingJournal applies the same path policy as resolveJournal but
// never creates the data directory. Read-only commands use it so a typo cannot
// leave behind a directory, journal, or sidecar lock.
func resolveExistingJournal(dataDirFlag, journal string, journalSet bool) (path, dataDir string, defaulted bool, err error) {
	if journalSet {
		if journal == "" {
			return "", "", false, fmt.Errorf("journal is required")
		}
		return journal, "", false, nil
	}
	dataDir, err = commandDataDir(dataDirFlag)
	if err != nil {
		return "", "", false, err
	}
	return filepath.Join(dataDir, "journal.ndjson"), dataDir, true, nil
}
