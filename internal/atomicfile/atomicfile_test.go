package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file is in its folder whole, readable by everyone, or not at all; and
// nothing of it is ever in that folder under another name.
func TestAFileLandsWholeOrNotAtAll(t *testing.T) {
	tmp, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(dst, "a.csv")
	if err := Write(tmp, path, func(w io.Writer) error {
		_, err := io.WriteString(w, "whole\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "whole\n" {
		t.Errorf("a.csv holds %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != Mode {
		t.Errorf("a.csv is %v, want %v", st.Mode().Perm(), os.FileMode(Mode))
	}

	gaveOut := errors.New("gave out")
	err := Write(tmp, filepath.Join(dst, "b.csv"), func(w io.Writer) error {
		io.WriteString(w, strings.Repeat("half", 1000))
		return gaveOut
	})
	if !errors.Is(err, gaveOut) {
		t.Fatalf("a write that gave out: %v", err)
	}
	for dir, want := range map[string]int{tmp: 0, dst: 1} {
		if entries, _ := os.ReadDir(dir); len(entries) != want {
			t.Errorf("%s holds %v, want %d file(s)", dir, entries, want)
		}
	}
}

// Discard after Commit leaves the committed file; a sweep takes what a
// process that stopped mid-write left behind, and nothing else.
func TestDiscardAndSweep(t *testing.T) {
	tmp, dst := t.TempDir(), t.TempDir()
	f, err := Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(filepath.Join(dst, "kept")); err != nil {
		t.Fatal(err)
	}
	f.Discard()
	if _, err := os.Stat(filepath.Join(dst, "kept")); err != nil {
		t.Fatalf("Discard after Commit took the file: %v", err)
	}

	left, err := Create(tmp) // a process that stopped here
	if err != nil {
		t.Fatal(err)
	}
	left.Close()
	if err := os.WriteFile(filepath.Join(tmp, "not-ours"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := Sweep(tmp); n != 1 || err != nil {
		t.Errorf("swept %d (%v), want the one left behind", n, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "not-ours")); err != nil {
		t.Errorf("the sweep took a file that is not a temporary one: %v", err)
	}
}
