package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// fileCache keeps shipped files by content hash so repeated jobs don't
// resend them. Every entry is verified against its hash before it is stored,
// so a caller cannot plant content under someone else's hash.
type fileCache struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
}

func newFileCache(dir string, maxBytes int64) (*fileCache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	// Uploads interrupted by a crash leave temp files behind.
	if stale, err := filepath.Glob(filepath.Join(dir, "incoming-*")); err == nil {
		for _, p := range stale {
			os.Remove(p)
		}
	}
	return &fileCache{dir: dir, maxBytes: maxBytes}, nil
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func (c *fileCache) path(hash string) string { return filepath.Join(c.dir, hash) }

func (c *fileCache) has(hash string) bool {
	if !validHash(hash) {
		return false
	}
	_, err := os.Stat(c.path(hash))
	return err == nil
}

// put stores data under hash after checking that they match.
func (c *fileCache) put(hash string, data []byte) error {
	if !validHash(hash) {
		return fmt.Errorf("invalid sha256 %q", hash)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return fmt.Errorf("content does not match sha256 %s", hash)
	}
	if c.verify(hash) {
		return nil
	}
	tmp, err := os.CreateTemp(c.dir, "incoming-")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("write cache entry: %v %v", werr, cerr)
	}
	if err := os.Rename(tmp.Name(), c.path(hash)); err != nil {
		os.Remove(tmp.Name())
		if c.has(hash) { // another job stored it first
			return nil
		}
		return err
	}
	c.evict()
	return nil
}

// verify reports whether the entry for hash exists and still matches it. An
// entry that no longer matches is removed, so the client is asked to resend it.
func (c *fileCache) verify(hash string) bool {
	if !validHash(hash) {
		return false
	}
	f, err := os.Open(c.path(hash))
	if err != nil {
		return false
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return false
	}
	if hex.EncodeToString(h.Sum(nil)) == hash {
		return true
	}
	os.Remove(c.path(hash))
	return false
}

// linkTo gives dst the cached file as a hard link, or a copy where links
// aren't supported. Only a job's program is linked: Windows Defender scans
// every new executable file before it runs (about half a second each on a
// laptop) but not another link to a file it has already scanned. A running
// program can't be written to on Windows or Linux, and missingFiles verifies
// the entry before every use, so a job that altered its program can't affect
// the next one.
func (c *fileCache) linkTo(hash, dst string) error {
	if !validHash(hash) {
		return fmt.Errorf("invalid sha256 %q", hash)
	}
	src := c.path(hash)
	_ = os.Chmod(src, 0o700) // links share permissions, and programs must be executable
	if err := os.Link(src, dst); err != nil {
		return c.copyTo(hash, dst)
	}
	now := time.Now()
	_ = os.Chtimes(src, now, now) // recently used entries survive eviction
	return nil
}

// copyTo copies a cached file to dst. Input files get a copy, never a link,
// so a job that modifies its input cannot corrupt the cache or the input of
// another job running at the same time.
func (c *fileCache) copyTo(hash, dst string) error {
	if !validHash(hash) {
		return fmt.Errorf("invalid sha256 %q", hash)
	}
	src, err := os.Open(c.path(hash))
	if err != nil {
		return fmt.Errorf("cached file %s: %w", hash[:12], err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	now := time.Now()
	_ = os.Chtimes(c.path(hash), now, now) // recently used entries survive eviction
	return out.Close()
}

// evict deletes the least recently used entries while the cache is over its
// size limit.
func (c *fileCache) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type entry struct {
		name string
		size int64
		mod  time.Time
	}
	var all []entry
	var total int64
	for _, e := range entries {
		if !validHash(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		all = append(all, entry{e.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, e := range all {
		if total <= c.maxBytes {
			return
		}
		if os.Remove(filepath.Join(c.dir, e.name)) == nil {
			total -= e.size
		}
	}
}
