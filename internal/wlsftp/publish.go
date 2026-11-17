package wlsftp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// PublishFrom is Publish for a file read from src, which it reads from the
// start once per folder: a file of hundreds of megabytes goes from disk to
// disk and is never held whole.
//
// Each copy is written under a hidden name beside its final one and
// renamed into place when it is whole, so a platform polling mid-publish
// sees the file complete or not at all -- never the first half of it,
// which is what a large file written in place would offer.
func PublishFrom(root, filename string, src io.ReadSeeker) (string, error) {
	for _, d := range []string{DirDownload, DirToWLNordic} {
		dir := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("wlsftp: create %s: %w", dir, err)
		}
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return "", fmt.Errorf("wlsftp: rewind %s: %w", filename, err)
		}
		if err := writeWhole(dir, filename, src); err != nil {
			return "", err
		}
	}
	return filename, nil
}

// writeWhole writes src to dir/filename by way of a hidden partial file in
// dir, renamed over filename once it is complete.
func writeWhole(dir, filename string, src io.Reader) error {
	final := filepath.Join(dir, filename)
	tmp, err := os.CreateTemp(dir, "."+filename+".*.partial")
	if err != nil {
		return fmt.Errorf("wlsftp: write %s: %w", final, err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("wlsftp: write %s: %w", final, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("wlsftp: write %s: %w", final, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wlsftp: write %s: %w", final, err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("wlsftp: write %s: %w", final, err)
	}
	ok = true
	return nil
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
