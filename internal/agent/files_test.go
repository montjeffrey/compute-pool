package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

func TestCollectDoesNotFollowLinksOutOfJobDir(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, pat := range []string{"out/secret.txt", "out/*", "out"} {
		var buf bytes.Buffer
		err := collectOutputs(dir, []string{pat}, proto.NewFrameWriter(&buf, nil))
		if err == nil || !strings.Contains(err.Error(), "nothing matched") {
			t.Errorf("collect %q: err = %v, want nothing matched", pat, err)
		}
		if strings.Contains(buf.String(), "c2VjcmV0") { // base64 of "secret"
			t.Errorf("collect %q returned a file from outside the job directory", pat)
		}
	}

	var buf bytes.Buffer
	if err := collectOutputs(dir, []string{"ok.txt"}, proto.NewFrameWriter(&buf, nil)); err != nil {
		t.Fatalf("collect ok.txt: %v", err)
	}
	if !strings.Contains(buf.String(), "ok.txt") {
		t.Errorf("ok.txt was not sent: %s", buf.String())
	}
}

func TestWriteInputsRejectsDuplicatePaths(t *testing.T) {
	cache, err := newFileCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	good := []byte("program")
	sum := sha256.Sum256(good)
	hash := hex.EncodeToString(sum[:])
	if err := cache.put(hash, good); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	files := []proto.InputFile{
		{Path: "helper.exe", SHA256: hash, Cached: true},
		{Path: "./helper.exe", Data: []byte("POISON")},
	}
	_, err = writeInputs(dir, files, cache, "helper.exe")
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("err = %v, want a duplicate-path refusal", err)
	}
	got, err := os.ReadFile(filepath.Join(cache.dir, hash))
	if err != nil || !bytes.Equal(got, good) {
		t.Errorf("cache entry = %q, %v; want it unchanged", got, err)
	}
}

func TestCacheRequiresLowercaseHashes(t *testing.T) {
	cache, err := newFileCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("x")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := cache.put(hash, data); err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(hash)
	if validHash(upper) || cache.has(upper) || cache.verify(upper) {
		t.Error("an uppercase hash was accepted")
	}
	if err := cache.put(upper, data); err == nil {
		t.Error("put accepted an uppercase hash")
	}
	if !cache.has(hash) {
		t.Error("the lowercase entry was lost")
	}
}

func TestStaleCleanupOnlyRemovesJobFolders(t *testing.T) {
	work := t.TempDir()
	for _, d := range []string{"job-123456", "job-7", "job-tracker", "job-", "jobs", "notes"} {
		if err := os.MkdirAll(filepath.Join(work, d), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, d, "f.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "job-99"), []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeStaleJobDirs(work)
	for _, gone := range []string{"job-123456", "job-7"} {
		if _, err := os.Stat(filepath.Join(work, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was not removed", gone)
		}
	}
	for _, kept := range []string{"job-tracker", "job-", "jobs", "notes", "job-99"} {
		if _, err := os.Stat(filepath.Join(work, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
}

func TestCollectReportsAPatternThatMatchesOnlyALink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	var buf bytes.Buffer
	err := collectOutputs(dir, []string{"link.txt"}, proto.NewFrameWriter(&buf, nil))
	if err == nil || !strings.Contains(err.Error(), "links are not collected") {
		t.Errorf("err = %v, want a note that links are not collected", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a link was sent: %s", buf.String())
	}
	// A pattern that matches a link and a real file is satisfied by the file.
	buf.Reset()
	if err := collectOutputs(dir, []string{"*.txt"}, proto.NewFrameWriter(&buf, nil)); err != nil {
		t.Errorf("*.txt: %v", err)
	}
}

// The input writers must never open an existing file, so a hard link to a
// cache entry can't be written through. This fails if O_EXCL is dropped.
func TestInputWritersNeverOverwriteAnExistingFile(t *testing.T) {
	cache, err := newFileCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	good := []byte("cached")
	sum := sha256.Sum256(good)
	hash := hex.EncodeToString(sum[:])
	if err := cache.put(hash, good); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	existing := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	check := func(what, p string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: overwrote an existing file", what)
		}
		if got, _ := os.ReadFile(p); string(got) != "original" {
			t.Errorf("%s: file now holds %q", what, got)
		}
	}
	p := existing("a")
	check("writeNew", p, writeNew(p, []byte("POISON")))
	p = existing("b")
	check("copyTo", p, cache.copyTo(hash, p))

	// Through writeInputs, with a file already in the job folder.
	p = existing("c.txt")
	_, err = writeInputs(dir, []proto.InputFile{{Path: "c.txt", Data: []byte("POISON")}}, cache, "")
	check("writeInputs", p, err)
}
