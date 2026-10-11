package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/montjeffrey/compute-pool/internal/proto"
)

var errCollectTooBig = fmt.Errorf("collected files exceed %d MiB", proto.MaxCollectBytes>>20)

// writeInputs writes shipped files into the job directory and returns their
// absolute paths keyed by cleaned relative path. Files sent with a hash are
// verified and added to cache; files marked Cached are copied from it.
func writeInputs(dir string, files []proto.InputFile, cache *fileCache) (map[string]string, error) {
	written := make(map[string]string, len(files))
	for _, f := range files {
		rel, err := proto.CleanRelPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("input file: %w", err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		switch {
		case f.Cached:
			if cache == nil {
				return nil, errors.New("input file: caching is off on this agent")
			}
			if err := cache.copyTo(f.SHA256, dst); err != nil {
				return nil, fmt.Errorf("input file %s: %w", rel, err)
			}
		default:
			if f.SHA256 != "" && cache != nil {
				if err := cache.put(f.SHA256, f.Data); err != nil {
					return nil, fmt.Errorf("input file %s: %w", rel, err)
				}
			}
			// Owner-executable so shipped programs and scripts can run.
			if err := os.WriteFile(dst, f.Data, 0o700); err != nil {
				return nil, err
			}
		}
		written[rel] = dst
	}
	return written, nil
}

// resolveProgram points argv[0] at a shipped file when it names one, so
// `--ship tool.exe -- tool.exe` runs the shipped copy rather than searching
// PATH (which never includes the job directory).
func resolveProgram(argv0 string, shipped map[string]string) string {
	rel, err := proto.CleanRelPath(argv0)
	if err != nil {
		return argv0
	}
	if p, ok := shipped[rel]; ok {
		return p
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(rel), ".exe") {
		if p, ok := shipped[rel+".exe"]; ok {
			return p
		}
	}
	return argv0
}

// collectOutputs sends every regular file matching patterns back as file
// frames. Symbolic links and other special files are skipped so a job cannot
// point the agent at data outside its directory.
func collectOutputs(dir string, patterns []string, fw *proto.FrameWriter) error {
	seen := make(map[string]bool)
	var total int64
	var unmatched []string
	for _, pat := range patterns {
		rel, err := proto.CleanRelPath(pat)
		if err != nil {
			return err
		}
		// Match within the job directory itself, so glob characters in the
		// directory's own path can't change what the pattern selects.
		matches, err := fs.Glob(os.DirFS(dir), rel)
		if err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
		if len(matches) == 0 {
			unmatched = append(unmatched, pat)
		}
		for _, m := range matches {
			full := filepath.Join(dir, filepath.FromSlash(m))
			err := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.Type().IsRegular() {
					return nil // directories are walked; links and devices are skipped
				}
				r, err := filepath.Rel(dir, p)
				if err != nil {
					return nil
				}
				slash, err := proto.CleanRelPath(filepath.ToSlash(r))
				if err != nil || seen[slash] {
					return nil
				}
				seen[slash] = true
				info, err := d.Info()
				if err != nil {
					return err
				}
				if total += info.Size(); total > proto.MaxCollectBytes {
					return errCollectTooBig
				}
				return sendFile(p, slash, fw)
			})
			if err != nil {
				return err
			}
		}
	}
	if len(unmatched) > 0 {
		return fmt.Errorf("nothing matched %s", strings.Join(unmatched, ", "))
	}
	return nil
}

func sendFile(src, rel string, fw *proto.FrameWriter) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, proto.FileChunkBytes)
	sent := false
	for {
		n, err := f.Read(buf)
		if n > 0 || (!sent && errors.Is(err, io.EOF)) {
			// An empty file still gets one frame so the client creates it.
			if werr := fw.Write(proto.Frame{Type: proto.FrameFile, Path: rel, Data: buf[:n]}); werr != nil {
				return werr
			}
			sent = true
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
