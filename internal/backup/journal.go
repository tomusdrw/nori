package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// writeJournal publishes a recovery record atomically, independently of SQLite.
func (m *Manager) writeJournal(name string, value any) error {
	file, err := os.CreateTemp(m.dir, ".nori-write-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := errors.Join(json.NewEncoder(file).Encode(value), file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(m.dir, name)); err != nil {
		return err
	}
	dir, err := os.Open(m.dir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
