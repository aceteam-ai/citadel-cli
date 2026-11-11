package composerefresh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// MaterializeMissing creates only absent embedded files, independently of the
// version stamp. Existing files, including symlinks and custom flavors, are not
// opened or changed. Callers hold their normal node mutation/training guard.
// Exclusive creation refuses overwrite even if another writer wins the race.
// This prepares files only; it never probes hardware or starts a service.
func MaterializeMissing(servicesDir string, embedded map[string]string) ([]string, error) {
	if len(embedded) == 0 {
		return nil, nil
	}
	if servicesDir == "" {
		return nil, errors.New("missing services directory")
	}
	names := make([]string, 0, len(embedded))
	for name, content := range embedded {
		if name == "" || content == "" {
			return nil, errors.New("invalid embedded compose")
		}
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return nil, errors.New("invalid embedded compose name")
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if err := os.MkdirAll(servicesDir, 0755); err != nil {
		return nil, err
	}
	var created []string
	for _, name := range names {
		path := filepath.Join(servicesDir, name+".yml")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return created, fmt.Errorf("create missing compose for %s: %w", name, err)
		}
		owned, statErr := file.Stat()
		_, writeErr := io.WriteString(file, embedded[name])
		closeErr := file.Close()
		if err := errors.Join(statErr, writeErr, closeErr); err != nil {
			// Do not remove a replacement installed by a competing writer.
			current, checkErr := os.Lstat(path)
			if statErr == nil && checkErr == nil && os.SameFile(owned, current) {
				_ = os.Remove(path)
			}
			return created, fmt.Errorf("write missing compose for %s: %w", name, err)
		}
		created = append(created, name)
	}
	return created, nil
}
