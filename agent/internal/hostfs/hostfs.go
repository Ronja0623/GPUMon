// Package hostfs abstracts read access to host files such as /proc and /etc,
// so that tests and the simulator can use an in-memory filesystem.
package hostfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
)

// FS is the read-only view of the host filesystem used by the agent.
// Paths are absolute, for example "/proc/1/stat".
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]string, error)
	Readlink(name string) (string, error)
}

// OS reads the real host filesystem.
type OS struct{}

// ReadFile reads a whole file.
func (OS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// ReadDir returns the entry names of a directory.
func (OS) ReadDir(name string) ([]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// Readlink returns the target of a symbolic link.
func (OS) Readlink(name string) (string, error) { return os.Readlink(name) }

// MapFS is an in-memory FS. It is safe for concurrent use.
type MapFS struct {
	mu    sync.RWMutex
	files map[string][]byte
	links map[string]string
	dirs  map[string]map[string]struct{}
}

// NewMapFS returns an empty in-memory filesystem.
func NewMapFS() *MapFS {
	return &MapFS{files: map[string][]byte{}, links: map[string]string{}, dirs: map[string]map[string]struct{}{}}
}

func (m *MapFS) addParents(name string) {
	for name != "/" && name != "." {
		dir, base := path.Split(name)
		dir = path.Clean(dir)
		children, ok := m.dirs[dir]
		if !ok {
			children = map[string]struct{}{}
			m.dirs[dir] = children
		}
		if _, ok := children[base]; ok {
			return
		}
		children[base] = struct{}{}
		name = dir
	}
}

// WriteFile stores a file.
func (m *MapFS) WriteFile(name string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = path.Clean(name)
	m.files[name] = data
	m.addParents(name)
}

// WriteString stores a file from a string.
func (m *MapFS) WriteString(name, data string) { m.WriteFile(name, []byte(data)) }

// Symlink stores a symbolic link.
func (m *MapFS) Symlink(name, target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = path.Clean(name)
	m.links[name] = target
	m.addParents(name)
}

// RemoveAll removes name and everything below it.
func (m *MapFS) RemoveAll(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name = path.Clean(name)
	prefix := name + "/"
	for k := range m.files {
		if k == name || strings.HasPrefix(k, prefix) {
			delete(m.files, k)
		}
	}
	for k := range m.links {
		if k == name || strings.HasPrefix(k, prefix) {
			delete(m.links, k)
		}
	}
	for k := range m.dirs {
		if k == name || strings.HasPrefix(k, prefix) {
			delete(m.dirs, k)
		}
	}
	dir, base := path.Split(name)
	if children, ok := m.dirs[path.Clean(dir)]; ok {
		delete(children, base)
	}
}

func notExist(op, name string) error {
	return &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
}

// ReadFile returns a copy of a stored file.
func (m *MapFS) ReadFile(name string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.files[path.Clean(name)]
	if !ok {
		return nil, notExist("open", name)
	}
	return append([]byte(nil), b...), nil
}

// ReadDir returns the sorted entry names of a directory.
func (m *MapFS) ReadDir(name string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	children, ok := m.dirs[path.Clean(name)]
	if !ok {
		return nil, notExist("open", name)
	}
	out := make([]string, 0, len(children))
	for c := range children {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// Readlink returns a stored link target.
func (m *MapFS) Readlink(name string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.links[path.Clean(name)]
	if !ok {
		return "", notExist("readlink", name)
	}
	return t, nil
}

// IsNotExist reports whether err means the file does not exist.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
