package wlsftp

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingAfter reads from r and fails once n bytes are read: a disk or a
// body that gives out halfway.
type failingAfter struct {
	r io.ReadSeeker
	n int64
}

func (f *failingAfter) Read(p []byte) (int, error) {
	pos, _ := f.r.Seek(0, io.SeekCurrent)
	if pos >= f.n {
		return 0, errors.New("gave out")
	}
	if rest := f.n - pos; int64(len(p)) > rest {
		p = p[:rest]
	}
	return f.r.Read(p)
}

func (f *failingAfter) Seek(off int64, whence int) (int64, error) { return f.r.Seek(off, whence) }

// PublishFrom puts the whole file in both folders and nothing beside it;
// one that fails halfway leaves neither the file nor a partial of it, so a
// platform polling the folder never sees half a file.
func TestPublishFromIsWholeOrNothing(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("row\n", 100_000)

	if _, err := PublishFrom(root, "a.csv", strings.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{DirDownload, DirToWLNordic} {
		entries, _ := os.ReadDir(filepath.Join(root, d))
		if len(entries) != 1 || entries[0].Name() != "a.csv" {
			t.Fatalf("%s holds %v, want only a.csv", d, entries)
		}
		got, _ := os.ReadFile(filepath.Join(root, d, "a.csv"))
		if string(got) != content {
			t.Errorf("%s/a.csv is %d bytes, want %d", d, len(got), len(content))
		}
		if st, _ := os.Stat(filepath.Join(root, d, "a.csv")); st.Mode().Perm() != 0o644 {
			t.Errorf("%s/a.csv is %v, want 0644", d, st.Mode().Perm())
		}
	}

	src := &failingAfter{r: strings.NewReader(content), n: int64(len(content) / 2)}
	if _, err := PublishFrom(root, "b.csv", src); err == nil {
		t.Fatal("a publish whose source gave out halfway succeeded")
	}
	for _, d := range []string{DirDownload, DirToWLNordic} {
		entries, _ := os.ReadDir(filepath.Join(root, d))
		if len(entries) != 1 || entries[0].Name() != "a.csv" {
			t.Errorf("after a failed publish %s holds %v, want only a.csv", d, entries)
		}
	}
}
