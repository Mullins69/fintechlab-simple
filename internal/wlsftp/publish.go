package wlsftp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/webduvet/fintechlab-simple/internal/atomicfile"
)

// Publish writes one settlement file into the SFTP server's own
// directories under filename, plain, and returns the name it has there.
// Worldline leaves its settlement files on the SFT channel unencrypted (it
// said so late on, and the platform's acquirer-fts reads them as they
// come); PGP is for the enrolment exchange only.
//
// This replaces the previous arrangement, where the platform wrote plain
// files into a bind mount shared with the SFTP server and a background
// poller mirrored them across. That made the platform the producer of a
// file it was meant to be receiving, and meant nothing ever exercised the
// download path. Here the acquirer writes into its own root and the
// platform's only route to the content is over the wire.
//
// The file lands in both real path conventions (download/ and
// to_WLNORDIC/): the same content is reachable under either, which is what
// a real Worldline SFT account exposes.
func Publish(root, filename string, body []byte) (string, error) {
	return PublishFrom(root, filename, bytes.NewReader(body))
}

// PublishFrom is Publish for a file read from src: Stage, then Commit.
func PublishFrom(root, filename string, src io.Reader) (string, error) {
	s, err := Stage(root, filename, func(w io.Writer) error {
		_, err := io.Copy(w, src)
		return err
	})
	if err != nil {
		return "", err
	}
	defer s.Discard()
	return s.Commit()
}

// DirStaging is where a file is written before it is renamed into the
// folders a platform lists: a hidden folder of the root's own, beside
// download/ and to_WLNORDIC/ and never inside either, so a poll of those
// folders mid-publish meets the file whole or not at all -- never half of
// it under a hidden name, which a poller that takes every name it lists
// would take for a settlement file. It is inside the root, not beside it,
// because a rename is only whole within one filesystem.
const DirStaging = ".staging"

// Staged is a settlement file written whole into the root's staging
// folder, one copy for each folder it is published in, and not yet in
// any of them.
type Staged struct {
	root, filename string
	copies         map[string]*atomicfile.File // by folder
}

// Stage writes the file write renders into root's staging folder, once
// for download/ and once for to_WLNORDIC/, without publishing it: a file
// of hundreds of megabytes takes as long as its disk does, and none of it
// is visible until Commit. write is called once; the second copy is made
// from the first.
func Stage(root, filename string, write func(io.Writer) error) (*Staged, error) {
	s := &Staged{root: root, filename: filename, copies: map[string]*atomicfile.File{}}
	tmp := filepath.Join(root, DirStaging)
	first, err := atomicfile.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("wlsftp: stage %s: %w", filename, err)
	}
	s.copies[DirDownload] = first
	if err := write(first); err != nil {
		s.Discard()
		return nil, fmt.Errorf("wlsftp: stage %s: %w", filename, err)
	}
	second, err := atomicfile.Create(tmp)
	if err != nil {
		s.Discard()
		return nil, fmt.Errorf("wlsftp: stage %s: %w", filename, err)
	}
	s.copies[DirToWLNordic] = second
	if _, err := first.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(second, first)
	}
	if err != nil {
		s.Discard()
		return nil, fmt.Errorf("wlsftp: stage %s: %w", filename, err)
	}
	return s, nil
}

// Commit publishes what Stage wrote: each copy is renamed into its folder,
// which takes a moment whatever the file's size. It returns the name the
// file has there.
func (s *Staged) Commit() (string, error) {
	for _, d := range []string{DirDownload, DirToWLNordic} {
		dir := filepath.Join(s.root, filepath.FromSlash(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("wlsftp: create %s: %w", dir, err)
		}
		if err := s.copies[d].Commit(filepath.Join(dir, s.filename)); err != nil {
			return "", fmt.Errorf("wlsftp: publish %s: %w", s.filename, err)
		}
	}
	return s.filename, nil
}

// Discard removes whatever copy Commit did not publish. It is safe to
// defer and to call more than once.
func (s *Staged) Discard() {
	for _, f := range s.copies {
		f.Discard()
	}
}

// SweepStaging removes what a publish left in root's staging folder when
// the process stopped mid-way. Call it where nothing can be publishing, at
// start-up.
func SweepStaging(root string) (int, error) {
	return atomicfile.Sweep(filepath.Join(root, DirStaging))
}

// RemoveFiles deletes the regular files directly in root/dir whose name
// ours accepts, and returns how many it deleted. It is how a reset takes
// back what the acquirer wrote into a directory it shares with the
// platform: the test is the caller's proof of authorship, and anything it
// does not accept stays. It never deletes a directory, a symlink or
// anything below dir. A missing dir is nothing to delete; a file that
// disappears before it can be deleted (the platform took it) is not
// counted. Every other failure is returned, after the rest are tried.
func RemoveFiles(root, dir string, ours func(name string) bool) (int, error) {
	full := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("wlsftp: read %s: %w", full, err)
	}
	var n int
	var errs []error
	for _, e := range entries {
		if !e.Type().IsRegular() || !ours(e.Name()) {
			continue
		}
		p := filepath.Join(full, e.Name())
		if err := os.Remove(p); err != nil {
			if !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("wlsftp: remove %s: %w", p, err))
			}
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
