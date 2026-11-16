package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/artemidius/insteadly/internal/core/domain"
	"github.com/artemidius/insteadly/internal/core/ports"
)

// blobKinds is the allowlist of blob kinds a caller may address.
var blobKinds = map[string]struct{}{
	domain.BlobKindRecording:  {},
	domain.BlobKindTranscript: {},
}

// systemBlobDir is the bucket for blobs not tied to an order. It cannot collide with
// an order ID (those are 32 lowercase hex chars).
const systemBlobDir = "_system"

// Put streams r into the blob addressed by key, creating directories as needed. The
// last write for a key wins.
func (s *Store) Put(ctx context.Context, key domain.BlobKey, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.blobDir(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("blob %s/%s: create dir: %w", key.Kind, key.Name, err)
	}
	path := filepath.Join(dir, key.Name)
	if err := writeAtomic(path, 0o600, func(w io.Writer) error {
		if _, err := io.Copy(w, r); err != nil {
			return fmt.Errorf("blob %s/%s: write: %w", key.Kind, key.Name, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("blob %s/%s: %w", key.Kind, key.Name, err)
	}
	return nil
}

// Open returns a reader for the blob, or an error wrapping ports.ErrNotFound.
func (s *Store) Open(ctx context.Context, key domain.BlobKey) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.blobDir(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, key.Name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("blob %s/%s: %w", key.Kind, key.Name, ports.ErrNotFound)
		}
		return nil, fmt.Errorf("blob %s/%s: open: %w", key.Kind, key.Name, err)
	}
	return f, nil
}

// Delete removes a blob. Deleting a missing blob is not an error (idempotent).
func (s *Store) Delete(ctx context.Context, key domain.BlobKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.blobDir(key)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, key.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob %s/%s: delete: %w", key.Kind, key.Name, err)
	}
	return nil
}

// blobDir validates key and returns the directory that holds it. Every component that
// becomes part of a path is checked here — this is the traversal guard.
func (s *Store) blobDir(key domain.BlobKey) (string, error) {
	if _, ok := blobKinds[key.Kind]; !ok {
		return "", fmt.Errorf("invalid blob kind %q", key.Kind)
	}
	if !validBlobName(key.Name) {
		return "", fmt.Errorf("invalid blob name %q", key.Name)
	}
	idDir := systemBlobDir
	if key.OrderID != "" {
		if !validOrderID(key.OrderID) {
			return "", fmt.Errorf("invalid order id %q", key.OrderID)
		}
		idDir = string(key.OrderID)
	}
	return filepath.Join(s.dir, "blobs", idDir, key.Kind), nil
}

// validBlobName accepts a plain filename only: non-empty, no path separators, not a
// dot entry, and equal to its own base name.
func validBlobName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return false
	}
	return filepath.Base(name) == name
}
