package logcollector

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Storage is the interface for log persistence.
type Storage interface {
	Write(path string, data io.Reader) error
	Read(path string) (io.ReadCloser, error)
	Delete(path string) error
	ListPrefix(prefix string) []string
	UsageBytes() (int64, error)
}

// DiskStorage implements Storage backed by a local directory (PV mount).
type DiskStorage struct {
	root string
}

func NewDiskStorage(root string) *DiskStorage {
	return &DiskStorage{root: root}
}

func (s *DiskStorage) Write(path string, data io.Reader) error {
	full := filepath.Join(s.root, filepath.Clean(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}

	f, err := os.Create(full)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := io.Copy(f, data); err != nil {
		return fmt.Errorf("writing file: %w", err)
	}
	return nil
}

func (s *DiskStorage) Read(path string) (io.ReadCloser, error) {
	full := filepath.Join(s.root, filepath.Clean(path))
	return os.Open(full)
}

func (s *DiskStorage) Delete(path string) error {
	full := filepath.Join(s.root, filepath.Clean(path))
	return os.RemoveAll(full)
}

func (s *DiskStorage) ListPrefix(prefix string) []string {
	full := filepath.Join(s.root, filepath.Clean(prefix))
	var result []string
	_ = filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(s.root, path)
			result = append(result, rel)
		}
		return nil
	})
	sort.Strings(result)
	return result
}

func (s *DiskStorage) UsageBytes() (int64, error) {
	var total int64
	err := filepath.Walk(s.root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// OldestFiles returns log files sorted by modification time (oldest first),
// grouped by investigation directory for pruning.
func (s *DiskStorage) OldestFiles() ([]string, error) {
	type fileEntry struct {
		path    string
		modTime int64
	}
	var entries []fileEntry

	err := filepath.Walk(s.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(s.root, path)
		if strings.HasSuffix(rel, ".log") {
			entries = append(entries, fileEntry{rel, info.ModTime().Unix()})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime < entries[j].modTime
	})

	result := make([]string, len(entries))
	for i, e := range entries {
		result[i] = e.path
	}
	return result, nil
}
