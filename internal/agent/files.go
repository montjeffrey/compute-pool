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
// verified and added to cache; files marked Cached are copied from it. The
// file at path program (see shippedProgram) is linked from the cache instead.
func writeInputs(dir string, files []proto.InputFile, cache *fileCache, program string) (map[string]string, error) {
	written := make(map[string]string, len(files))
	// Two entries for one file would make the second write through the hard
	// link the first one may have made into the cache.
	names := make(map[string]bool, len(files))
	for _, f := range files {
		rel, err := proto.CleanRelPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("input file: %w", err)
		}
		key := rel
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			key = strings.ToLower(rel) // these file systems ignore case
		}
		if names[key] {
			return nil, fmt.Errorf("input file %s: sent more than once", rel)
		}
		names[key] = true
		// CleanRelPath has already refused anything that leaves the job
		// directory. The path is checked once more in this system's own form,
		// right where it is joined, so the join never depends on a check made
		// elsewhere (and static analysis can see that it is contained).
		local := filepath.FromSlash(rel)
		if !filepath.IsLocal(local) {
			return nil, fmt.Errorf("input file: unsafe path %q", f.Path)
		}
		dst := filepath.Join(dir, local)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, err
		}
		switch {
		case f.Cached:
			if cache == nil {
				return nil, errors.New("input file: caching is off on this agent")
			}
			place := cache.copyTo
			if rel == program {
				place = cache.linkTo
			}
			if err := place(f.SHA256, dst); err != nil {
				return nil, fmt.Errorf("input file %s: %w", rel, err)
			}
		default:
			if f.SHA256 != "" && cache != nil {
				if err := cache.put(f.SHA256, f.Data); err != nil {
					return nil, fmt.Errorf("input file %s: %w", rel, err)
				}
				if rel == program {
					if err := cache.linkTo(f.SHA256, dst); err != nil {
						return nil, fmt.Errorf("input file %s: %w", rel, err)
					}
					break
				}
			}
			// Owner-executable so shipped programs and scripts can run.
			if err := writeNew(dst, f.Data); err != nil {
				return nil, err
			}
		}
		written[rel] = dst
	}
	return written, nil
}

// writeNew creates dst, which must not exist yet, so it can never write
// through a link into another file.
func writeNew(dst string, data []byte) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// shippedProgram returns the cleaned path of the shipped file that argv[0]
// names, so `--ship tool.exe -- tool.exe` runs the shipped copy rather than
// searching PATH (which never includes the job directory). It returns "" for
// shell jobs and when argv[0] names no shipped file.
func shippedProgram(req proto.RunRequest) string {
	if req.Shell || len(req.Argv) == 0 {
		return ""
	}
	rel, err := proto.CleanRelPath(req.Argv[0])
	if err != nil {
		return ""
	}
	names := make(map[string]bool, len(req.Files))
	for _, f := range req.Files {
		if p, err := proto.CleanRelPath(f.Path); err == nil {
			names[p] = true
		}
	}
	if names[rel] {
		return rel
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(rel), ".exe") && names[rel+".exe"] {
		return rel + ".exe"
	}
	return ""
}

// collectOutputs sends every regular file matching patterns back as file
// frames. The job directory is opened as a root, so no path component, not
// only the last, can lead outside it through a symbolic link or junction.
// Links and other special files are skipped.
func collectOutputs(dir string, patterns []string, fw *proto.FrameWriter) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	fsys := root.FS()
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
		matches, err := fs.Glob(fsys, rel)
		if err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
		usable, links := 0, 0
		for _, m := range matches {
			fi, err := root.Lstat(m)
			if err != nil || !fi.Mode().IsRegular() && !fi.IsDir() {
				if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
					links++
				}
				continue // links and devices are skipped
			}
			usable++
			err = fs.WalkDir(fsys, m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.Type().IsRegular() {
					return nil // directories are walked; links and devices are skipped
				}
				slash, err := proto.CleanRelPath(p)
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
				return sendFile(root, p, slash, fw)
			})
			if err != nil {
				return err
			}
		}
		switch {
		case usable > 0:
		case links > 0:
			unmatched = append(unmatched, pat+" (it names only links; links are not collected)")
		default:
			unmatched = append(unmatched, pat)
		}
	}
	if len(unmatched) > 0 {
		return fmt.Errorf("nothing matched %s", strings.Join(unmatched, ", "))
	}
	return nil
}

func sendFile(root *os.Root, name, rel string, fw *proto.FrameWriter) error {
	f, err := root.Open(name)
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
