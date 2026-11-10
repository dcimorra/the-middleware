// Package fsx holds the few filesystem primitives the whole process relies
// on. Everything that decides something is written through WriteAtomic so a
// crash between two syscalls can never leave a half-written manifest behind.
package fsx

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteAtomic writes data to path by writing a sibling temp file and renaming
// it over the target. Rename is atomic on POSIX filesystems, so a reader sees
// either the old content or the new one, never a prefix.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// WriteJSON marshals v with indentation and writes it atomically. Files
// people open with an editor are indented on purpose: a manifest is read by
// humans far more often than it is parsed.
func WriteJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(path, append(data, '\n'), perm)
}

// ReadJSON reads path into v. A missing file is reported as os.ErrNotExist so
// callers can distinguish "not there yet" from "broken".
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// ReadTrimmed returns the file's content without surrounding whitespace. It
// is what env_from_file uses: a pointer file written with `echo` carries a
// trailing newline that must not end up inside the variable.
func ReadTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Exists reports whether path exists, treating any other error as "no" so a
// permission problem reads as absence rather than crashing a scan.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// IsFile reports whether path is something other than a directory that
// can be stat'ed (a link is followed).
func IsFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// IsDir reports whether path is a folder, following links.
func IsDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// IsNotExist is errors.Is(err, os.ErrNotExist), named for readability at call
// sites that branch on it.
func IsNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
