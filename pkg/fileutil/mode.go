package fileutil

import "os"

// EnsureFileMode changes path's permission bits and synchronizes the metadata.
// It reports whether the permission bits changed.
func EnsureFileMode(path string, perm os.FileMode) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if info.Mode().Perm() == perm.Perm() {
		return false, nil
	}
	if err := os.Chmod(path, perm); err != nil {
		return false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return false, err
	}
	return true, nil
}
