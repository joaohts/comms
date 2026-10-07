package porter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Store persists State as a JSON file. Hooks run as short-lived processes, so
// every update takes an exclusive lock and replaces the file atomically.
type Store struct{ Path string }

func (s Store) lock() (*os.File, error) { return lockFile(s.Path) }

func lockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s Store) read() (*State, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	st := NewState()
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("read porter state %s: %w", s.Path, err)
	}
	if st.Agents == nil {
		st.Agents = map[string]*Agent{}
	}
	return st, nil
}

// Load returns the current state without modifying it.
func (s Store) Load() (*State, error) {
	l, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return s.read()
}

// Update runs fn on the current state and saves the result.
func (s Store) Update(fn func(*State) error) error {
	l, err := s.lock()
	if err != nil {
		return err
	}
	defer l.Close()
	st, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	return writeJSON(s.Path, st)
}

// LoadJSON reads path into a new T under the file's lock. A missing file
// yields the zero value.
func LoadJSON[T any](path string) (*T, error) {
	l, err := lockFile(path)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return readJSON[T](path)
}

// UpdateJSON runs fn on the current value of path under an exclusive lock and
// saves the result when fn reports a change.
func UpdateJSON[T any](path string, fn func(*T) (bool, error)) error {
	l, err := lockFile(path)
	if err != nil {
		return err
	}
	defer l.Close()
	v, err := readJSON[T](path)
	if err != nil {
		return err
	}
	changed, err := fn(v)
	if err != nil || !changed {
		return err
	}
	return writeJSON(path, v)
}

func readJSON[T any](path string) (*T, error) {
	v := new(T)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return v, nil
}

// WriteJSON replaces path atomically with the indented JSON of v.
func WriteJSON(path string, v any) error { return writeJSON(path, v) }

// writeJSON replaces path atomically with the indented JSON of v.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".porter-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
