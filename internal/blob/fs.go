package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed blob.Store for local development.
type FS struct {
	root string
}

// NewFS returns an FS rooted at root, creating it if needed.
func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("blob.FS: create root: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &FS{root: abs}, nil
}

// resolve turns a key into an absolute path, rejecting traversal attempts.
func (f *FS) resolve(key string) (string, error) {
	// Reject keys that start with .. or contain path components that escape root.
	if strings.Contains(key, "..") {
		return "", errors.New("blob.FS: key must not contain '..'")
	}
	p := filepath.Join(f.root, filepath.FromSlash(key))
	if !strings.HasPrefix(p, f.root+string(os.PathSeparator)) && p != f.root {
		return "", errors.New("blob.FS: key escapes root")
	}
	return p, nil
}

// Put writes data to the key atomically (temp file + rename).
func (f *FS) Put(_ context.Context, key string, data []byte) error {
	p, err := f.resolve(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("blob.FS Put mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob.FS Put temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob.FS Put write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob.FS Put close: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("blob.FS Put rename: %w", err)
	}
	return nil
}

// Get reads and returns the blob at key.
func (f *FS) Get(_ context.Context, key string) ([]byte, error) {
	p, err := f.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("blob.FS Get: %w", err)
	}
	return data, nil
}

// Delete removes the blob at key.
func (f *FS) Delete(_ context.Context, key string) error {
	p, err := f.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("blob.FS Delete: %w", err)
	}
	return nil
}
