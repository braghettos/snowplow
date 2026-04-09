package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"context"
	"time"
)

// DiskStore is a file-based key-value store for L1 resolved cache entries.
// Keys are hashed to file paths using SHA256. Data is stored as raw bytes
// (already zstd-compressed by the caller). TTL is enforced via file mtime
// and a background cleanup goroutine.
//
// Concurrency: writes are idempotent (same key always produces the same data),
// so no file locking is needed. A read during a write may see stale data,
// which is acceptable under the stale-while-revalidate model.
type DiskStore struct {
	baseDir string
	ttl     time.Duration
}

// NewDiskStore creates a new disk-backed store rooted at baseDir.
// The directory is created if it does not exist.
func NewDiskStore(baseDir string, ttl time.Duration) (*DiskStore, error) {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	return &DiskStore{baseDir: baseDir, ttl: ttl}, nil
}

// keyToPath converts a cache key to a file path using SHA256 hashing
// with 2-level directory sharding: {baseDir}/{hash[0:2]}/{hash[2:4]}/{hash}.bin
func (d *DiskStore) keyToPath(key string) string {
	h := sha256.Sum256([]byte(key))
	hex := hex.EncodeToString(h[:])
	return filepath.Join(d.baseDir, hex[0:2], hex[2:4], hex+".bin")
}

// Get reads the value for the given key from disk.
// Returns (data, true, nil) on hit, (nil, false, nil) on miss,
// or (nil, false, err) on I/O error.
// Expired files (mtime + TTL < now) are treated as misses.
func (d *DiskStore) Get(key string) ([]byte, bool, error) {
	path := d.keyToPath(key)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Check TTL based on mtime.
	if d.ttl > 0 && time.Since(info.ModTime()) > d.ttl {
		// Expired — treat as miss. Cleanup goroutine will remove the file.
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil // race: deleted between Stat and ReadFile
		}
		return nil, false, err
	}
	return data, true, nil
}

// Set writes data to disk for the given key. The parent directories are
// created as needed. Uses atomic write (temp file + rename) to avoid
// partial reads.
func (d *DiskStore) Set(key string, data []byte) error {
	path := d.keyToPath(key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Write to temp file in the same directory, then rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Delete removes files for the given keys. Missing files are not an error.
func (d *DiskStore) Delete(keys ...string) error {
	var firstErr error
	for _, key := range keys {
		path := d.keyToPath(key)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// StartCleanup runs a background goroutine that periodically scans the
// store directory and removes files older than TTL. The goroutine exits
// when ctx is cancelled.
func (d *DiskStore) StartCleanup(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.cleanup()
			}
		}
	}()
}

// cleanup walks the store directory and removes expired files.
// Also removes empty directories left behind after file deletion.
func (d *DiskStore) cleanup() {
	now := time.Now()
	removed := 0
	_ = filepath.WalkDir(d.baseDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if entry.IsDir() {
			return nil
		}
		// Only process .bin files (skip .tmp files from in-progress writes).
		if filepath.Ext(path) != ".bin" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if d.ttl > 0 && now.Sub(info.ModTime()) > d.ttl {
			if rerr := os.Remove(path); rerr == nil {
				removed++
			}
		}
		return nil
	})
	if removed > 0 {
		slog.Debug("diskstore: cleanup completed", slog.Int("removed", removed))
	}
	// Clean up empty directories (walk bottom-up by trying to remove).
	// This is best-effort — Readdirnames returns an error if not empty.
	_ = filepath.WalkDir(d.baseDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() || path == d.baseDir {
			return nil
		}
		// Try to remove; fails silently if not empty.
		_ = os.Remove(path)
		return nil
	})
}
