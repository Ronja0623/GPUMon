// Package disk reports statfs totals and runs the daily per-owner usage scan.
package disk

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gpumon/agent/internal/clock"
	"gpumon/agent/internal/payload"
)

// FileInfo is the subset of lstat(2) that the scanner uses.
type FileInfo struct {
	Dev       uint64
	Ino       uint64
	Nlink     uint64
	UID       uint32
	Size      int64 // apparent size in bytes (not used for accounting)
	Blocks    int64 // allocated 512-byte blocks
	IsDir     bool
	IsSymlink bool
}

// FS is the stat layer used by the scanner. It never follows symlinks.
type FS interface {
	Lstat(path string) (FileInfo, error)
	ReadDirNames(path string) ([]string, error)
}

// OSFS is the real filesystem.
type OSFS struct{}

// Lstat calls lstat(2).
func (OSFS) Lstat(p string) (FileInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return FileInfo{}, err
	}
	mode := st.Mode & syscall.S_IFMT
	return FileInfo{
		Dev:       uint64(st.Dev),
		Ino:       st.Ino,
		Nlink:     uint64(st.Nlink),
		UID:       st.Uid,
		Size:      st.Size,
		Blocks:    st.Blocks,
		IsDir:     mode == syscall.S_IFDIR,
		IsSymlink: mode == syscall.S_IFLNK,
	}, nil
}

// ReadDirNames lists a directory.
func (OSFS) ReadDirNames(p string) ([]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// MapFS is an in-memory FS for tests and simulation.
type MapFS struct {
	mu       sync.RWMutex
	entries  map[string]FileInfo
	children map[string][]string
	// OnLstat, when set, is called before every Lstat (tests use it to
	// advance a fake clock).
	OnLstat func(path string)
}

// NewMapFS returns an empty filesystem.
func NewMapFS() *MapFS {
	return &MapFS{entries: map[string]FileInfo{}, children: map[string][]string{}}
}

// Add stores an entry. Parent directories must be added separately.
func (m *MapFS) Add(p string, fi FileInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p = path.Clean(p)
	if _, exists := m.entries[p]; !exists && p != "/" {
		dir := path.Dir(p)
		m.children[dir] = append(m.children[dir], path.Base(p))
	}
	m.entries[p] = fi
}

// Lstat returns a stored entry.
func (m *MapFS) Lstat(p string) (FileInfo, error) {
	if m.OnLstat != nil {
		m.OnLstat(p)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	fi, ok := m.entries[path.Clean(p)]
	if !ok {
		return FileInfo{}, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
	}
	return fi, nil
}

// ReadDirNames lists stored children.
func (m *MapFS) ReadDirNames(p string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p = path.Clean(p)
	fi, ok := m.entries[p]
	if !ok || !fi.IsDir {
		return nil, &os.PathError{Op: "open", Path: p, Err: os.ErrNotExist}
	}
	return append([]string(nil), m.children[p]...), nil
}

// StatfsFunc returns statfs totals for a mount point.
type StatfsFunc func(mount string) (payload.Filesystem, error)

// OSStatfs calls statfs(2). Free is the space available to unprivileged users.
func OSStatfs(mount string) (payload.Filesystem, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(mount, &s); err != nil {
		return payload.Filesystem{}, err
	}
	bs := uint64(s.Bsize)
	return payload.Filesystem{
		Mount:     mount,
		SizeBytes: s.Blocks * bs,
		UsedBytes: (s.Blocks - s.Bfree) * bs,
		FreeBytes: s.Bavail * bs,
	}, nil
}

// MountOf returns the configured mount point that contains p (longest prefix).
func MountOf(p string, mounts []string) string {
	best := ""
	for _, m := range mounts {
		if p == m || m == "/" || strings.HasPrefix(p, strings.TrimSuffix(m, "/")+"/") {
			if len(m) > len(best) {
				best = m
			}
		}
	}
	if best == "" {
		return p
	}
	return best
}

// Scanner sums allocated blocks per owner below a set of paths.
type Scanner struct {
	FS      FS
	Owner   func(uid uint32) string
	Clock   clock.Clock
	Timeout time.Duration
	// Lower lowers the CPU and IO priority of the calling thread. Scan calls
	// it first; the agent runs Scan in a dedicated goroutine.
	Lower func() error
}

// Scan walks every path without crossing filesystems or following
// symlinks, counting each hard-linked inode once. When the timeout expires
// it stops and marks the result partial.
func (s *Scanner) Scan(ctx context.Context, paths []string, mountOf func(string) string) (payload.DiskScan, []string) {
	var errs []string
	if s.Lower != nil {
		if err := s.Lower(); err != nil {
			errs = append(errs, fmt.Sprintf("disk scan: cannot lower priority: %v", err))
		}
	}
	start := s.Clock.Now()
	deadline := start.Add(s.Timeout)
	res := payload.DiskScan{StartedAt: payload.FormatTime(start), Paths: []payload.ScanPath{}}
	expired := func() bool {
		if ctx.Err() != nil {
			return true
		}
		return s.Timeout > 0 && !s.Clock.Now().Before(deadline)
	}
	for _, root := range paths {
		sp, perr := s.scanOne(root, expired)
		if perr != "" {
			errs = append(errs, perr)
		}
		sp.Mount = mountOf(root)
		if sp.Partial {
			res.Partial = true
		}
		res.Paths = append(res.Paths, sp)
	}
	res.CompletedAt = payload.FormatTime(s.Clock.Now())
	return res, errs
}

func (s *Scanner) scanOne(root string, expired func() bool) (payload.ScanPath, string) {
	sp := payload.ScanPath{Path: root, Owners: []payload.OwnerUsage{}}
	rfi, err := s.FS.Lstat(root)
	if err != nil {
		sp.Partial = true
		return sp, fmt.Sprintf("disk scan: cannot stat %s: %v", root, err)
	}
	usage := map[uint32]uint64{}
	type inode struct{ dev, ino uint64 }
	seen := map[inode]struct{}{}
	count := func(fi FileInfo) {
		if !fi.IsDir && fi.Nlink > 1 {
			k := inode{fi.Dev, fi.Ino}
			if _, ok := seen[k]; ok {
				return
			}
			seen[k] = struct{}{}
		}
		if fi.Blocks > 0 {
			usage[fi.UID] += uint64(fi.Blocks) * 512
		}
	}
	count(rfi)
	unreadable := 0
	var stack []string
	if rfi.IsDir && !rfi.IsSymlink {
		stack = append(stack, root)
	}
walk:
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		names, err := s.FS.ReadDirNames(dir)
		if err != nil {
			unreadable++
			continue
		}
		for _, n := range names {
			if expired() {
				sp.Partial = true
				break walk
			}
			p := path.Join(dir, n)
			fi, err := s.FS.Lstat(p)
			if err != nil {
				unreadable++
				continue
			}
			if fi.Dev != rfi.Dev {
				continue // another filesystem is mounted here
			}
			count(fi)
			if fi.IsDir && !fi.IsSymlink {
				stack = append(stack, p)
			}
		}
	}
	byOwner := map[string]uint64{}
	for uid, b := range usage {
		byOwner[s.Owner(uid)] += b
	}
	for o, b := range byOwner {
		sp.Owners = append(sp.Owners, payload.OwnerUsage{Owner: o, Bytes: b})
		sp.TotalBytes += b
	}
	sort.Slice(sp.Owners, func(i, j int) bool {
		if sp.Owners[i].Bytes != sp.Owners[j].Bytes {
			return sp.Owners[i].Bytes > sp.Owners[j].Bytes
		}
		return sp.Owners[i].Owner < sp.Owners[j].Owner
	})
	msg := ""
	if unreadable > 0 {
		msg = fmt.Sprintf("disk scan: %d entries under %s could not be read", unreadable, root)
	}
	return sp, msg
}

// NextRun returns the next occurrence of the local time "HH:MM" after now.
func NextRun(now time.Time, hhmm string) time.Time {
	var h, m int
	_, _ = fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	t := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, 0, 0, now.Location())
	}
	return t
}
