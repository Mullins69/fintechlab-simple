// Package atomicfile writes a file under a temporary name and renames it to
// its own once it is whole, so whoever reads the folder it lands in sees
// the whole file or nothing -- never the first half of one still being
// written, which is what a large file written in place offers.
//
// The temporary file is made in a folder of the caller's choosing, apart
// from the one the file lands in, so that nothing listing that folder ever
// meets a file half-written under some other name either. The two folders
// must be on one filesystem: a rename is only whole within one.
package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Mode is what a committed file is made: readable by everyone, as a file
// written with os.WriteFile(…, 0o644) would be. os.CreateTemp's own 0600
// would leave a file another process -- an SFTP server, a platform -- can
// list and not read.
const Mode = 0o644

// File is a file being written under a temporary name.
type File struct {
	*os.File
	done bool
}

// Create starts a file in dir, made if it is missing. dir must be on the
// filesystem of the path Commit will name, and is best one nothing lists.
func Create(dir string) (*File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("atomicfile: %w", err)
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("atomicfile: %w", err)
	}
	return &File{File: f}, nil
}

// Commit makes the file path's, whole: it is made Mode, closed and renamed
// over path. Once it has been called, Discard does nothing; if it fails,
// the temporary file is removed.
func (f *File) Commit(path string) error {
	if f.done {
		return fmt.Errorf("atomicfile: %s: already committed or discarded", path)
	}
	err := f.Chmod(Mode)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	f.done = true
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("atomicfile: %s: %w", path, err)
	}
	return nil
}

// Discard removes the temporary file unless Commit made it its path's.
// It is safe to defer and to call more than once.
func (f *File) Discard() {
	if f.done {
		return
	}
	f.done = true
	f.Close()
	os.Remove(f.Name())
}

// Write writes what write produces to path, whole or not at all, by way of
// a temporary file in tmpDir.
func Write(tmpDir, path string, write func(io.Writer) error) error {
	f, err := Create(tmpDir)
	if err != nil {
		return err
	}
	defer f.Discard()
	if err := write(f); err != nil {
		return fmt.Errorf("atomicfile: %s: %w", path, err)
	}
	return f.Commit(path)
}

// Sweep removes what Create left in dir: the temporary files of a process
// that stopped before it committed or discarded them. Call it where
// nothing can be writing one, at start-up.
func Sweep(dir string) (int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range matches {
		if os.Remove(m) == nil {
			n++
		}
	}
	return n, nil
}
