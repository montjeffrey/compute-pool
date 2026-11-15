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
