package logbundles

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	bundleDir    = "/data/log-bundles"
	bundlePrefix = "logs-"
	bundleSuffix = ".tar.gz"
)

type Manager struct {
	dir string
}

func New() *Manager {
	return &Manager{dir: bundleDir}
}

// PruneOldBundles keeps the most recent `keep` bundles in the bundle directory
// and deletes older ones. Bundles are sorted by filename, which works because
// lsc names them logs-YYYY-MM-DD-HH-MM.tar.gz (lexicographic == chronological).
func (m *Manager) PruneOldBundles(keep int) error {
	bundles, err := m.list()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if len(bundles) <= keep {
		return nil
	}

	sort.Strings(bundles)
	toDelete := bundles[:len(bundles)-keep]

	for _, name := range toDelete {
		path := filepath.Join(m.dir, name)
		if err := os.Remove(path); err != nil {
			log.Printf("log bundles: failed to remove %s: %v", path, err)
			continue
		}
		log.Printf("log bundles: removed old bundle %s", name)
	}

	log.Printf("log bundles: kept %d most recent, removed %d", keep, len(toDelete))
	return nil
}

func (m *Manager) PrepareUSB(usbMountPath string) error {
	dest := filepath.Join(usbMountPath, "log-bundles")
	if err := os.MkdirAll(dest, 0755); err != nil {
		return fmt.Errorf("failed to create log-bundles directory: %w", err)
	}
	return nil
}

func (m *Manager) CopyToUSB(usbMountPath string) error {
	bundles, err := m.list()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	destDir := filepath.Join(usbMountPath, "log-bundles")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create log-bundles directory: %w", err)
	}

	copied := 0
	for _, name := range bundles {
		src := filepath.Join(m.dir, name)
		dst := filepath.Join(destDir, name)
		if err := copyFile(src, dst); err != nil {
			log.Printf("log bundles: failed to copy %s: %v", name, err)
			continue
		}
		copied++
	}

	if copied > 0 {
		log.Printf("log bundles: copied %d bundle(s) to USB drive", copied)
	}
	return nil
}

func (m *Manager) list() ([]string, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var bundles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, bundlePrefix) && strings.HasSuffix(name, bundleSuffix) {
			bundles = append(bundles, name)
		}
	}
	return bundles, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
