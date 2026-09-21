package fileutil

import (
	"io"
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path only after its complete contents are synced in
// a temporary file on the destination filesystem.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, perm, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// CopyFileAtomic copies src to a temporary file on the destination filesystem
// before replacing dst.
func CopyFileAtomic(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeAtomic(dst, perm, func(f *os.File) error {
		_, err := io.Copy(f, in)
		return err
	})
}

func writeAtomic(path string, perm os.FileMode, write func(*os.File) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmpPath := f.Name()
	published := false
	defer func() {
		if !published {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	published = true
	return nil
}
