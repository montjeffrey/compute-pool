package proto

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf, nil)
	want := []Frame{
		{Type: FrameStart, JobID: "j1", Node: "jm-c"},
		{Type: FrameStdout, Data: []byte("hello\n")},
		{Type: FrameStderr, Data: []byte{0x00, 0xff, '\n'}}, // binary-safe
		{Type: FrameError, Message: "boom"},
		{Type: FrameExit, ExitCode: 3, DurationMS: 42},
	}
	for _, f := range want {
		if err := fw.Write(f); err != nil {
			t.Fatal(err)
		}
	}

	fr := NewFrameReader(&buf)
	for i, w := range want {
		got, err := fr.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.Type != w.Type || !bytes.Equal(got.Data, w.Data) || got.JobID != w.JobID ||
			got.Node != w.Node || got.ExitCode != w.ExitCode || got.DurationMS != w.DurationMS || got.Message != w.Message {
			t.Errorf("frame %d = %+v, want %+v", i, got, w)
		}
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Errorf("after last frame: err = %v, want io.EOF", err)
	}
}

func TestStreamWriterConcurrentFramesStayIntact(t *testing.T) {
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf, nil)
	out, errw := fw.Stream(FrameStdout), fw.Stream(FrameStderr)

	var wg sync.WaitGroup
	for _, w := range []io.Writer{out, errw} {
		wg.Add(1)
		go func(w io.Writer) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, err := w.Write([]byte(strings.Repeat("x", 100))); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	fr := NewFrameReader(&buf)
	counts := map[FrameType]int{}
	for {
		f, err := fr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("corrupt stream after %v frames: %v", counts, err)
		}
		if len(f.Data) != 100 {
			t.Fatalf("frame data length %d, want 100", len(f.Data))
		}
		counts[f.Type]++
	}
	if counts[FrameStdout] != 200 || counts[FrameStderr] != 200 {
		t.Errorf("frame counts = %v, want 200 of each", counts)
	}
}

func TestCleanRelPath(t *testing.T) {
	ok := map[string]string{
		"a.txt":         "a.txt",
		"dir/b.txt":     "dir/b.txt",
		`dir\win\c.txt`: "dir/win/c.txt",
		"./x/../y.txt":  "y.txt",
		"out":           "out",
		"out/*.csv":     "out/*.csv",
	}
	for in, want := range ok {
		got, err := CleanRelPath(in)
		if err != nil || got != want {
			t.Errorf("CleanRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/abs", `\abs`, "C:x", `C:\x`, "file.txt:stream"} {
		if got, err := CleanRelPath(bad); err == nil {
			t.Errorf("CleanRelPath(%q) = %q, want error", bad, got)
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestFrameWriterStopsAfterFirstError(t *testing.T) {
	w := &failWriter{}
	fw := NewFrameWriter(w, nil)
	for i := 0; i < 3; i++ {
		if err := fw.Write(Frame{Type: FrameStdout}); err == nil {
			t.Fatal("Write succeeded on a broken stream")
		}
	}
	if w.n != 1 {
		t.Errorf("underlying writer called %d times, want 1", w.n)
	}
}
