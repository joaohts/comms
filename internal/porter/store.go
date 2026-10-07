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

func (s Store) lock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".porter-*.json")
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
	return os.Rename(tmp.Name(), s.Path)
}
