package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Store interface {
	Put([]byte) (string, error)
	Get(string) ([]byte, error)
}
type FileStore struct{ Dir string }

var _ Store = FileStore{}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s FileStore) Put(content []byte) (string, error) {
	if strings.TrimSpace(s.Dir) == "" {
		return "", fmt.Errorf("blob directory is required")
	}
	if len(content) == 0 {
		return "", fmt.Errorf("content is empty")
	}
	h := digest(content)
	path := filepath.Join(s.Dir, h)
	if old, err := os.ReadFile(path); err == nil {
		if digest(old) != h {
			return "", fmt.Errorf("existing blob hash mismatch")
		}
		return "sha256:" + h, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.Dir, ".blob-")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if old, readErr := os.ReadFile(path); readErr == nil && digest(old) == h {
			return "sha256:" + h, nil
		}
		return "", err
	}
	return "sha256:" + h, nil
}

func (s FileStore) Get(id string) ([]byte, error) {
	const prefix = "sha256:"
	h := strings.TrimPrefix(id, prefix)
	if !strings.HasPrefix(id, prefix) || len(h) != sha256.Size*2 {
		return nil, fmt.Errorf("invalid blob id")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return nil, fmt.Errorf("invalid blob id: %w", err)
	}
	if strings.TrimSpace(s.Dir) == "" {
		return nil, fmt.Errorf("blob directory is required")
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, h))
	if err != nil {
		return nil, err
	}
	if digest(b) != h {
		return nil, fmt.Errorf("blob content hash mismatch")
	}
	return append([]byte(nil), b...), nil
}
