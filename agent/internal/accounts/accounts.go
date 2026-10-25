// Package accounts parses /etc/passwd, /etc/subuid, and /etc/login.defs and
// resolves UIDs to usernames. Identity across servers is always the username.
package accounts

import (
	"fmt"
	"strconv"
	"strings"

	"gpumon/agent/internal/hostfs"
)

// DefaultUIDMin is used when neither the config nor /etc/login.defs sets UID_MIN.
const DefaultUIDMin = 1000

// Passwd maps UIDs to usernames for one host.
type Passwd struct {
	byUID  map[uint32]string
	byName map[string]uint32
}

// ParsePasswd parses passwd(5) content. Malformed lines are ignored.
func ParsePasswd(data []byte) *Passwd {
	p := &Passwd{byUID: map[uint32]string{}, byName: map[string]uint32{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		uid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		if _, dup := p.byUID[uint32(uid)]; !dup {
			p.byUID[uint32(uid)] = f[0]
		}
		p.byName[f[0]] = uint32(uid)
	}
	return p
}

// Name returns the username for uid.
func (p *Passwd) Name(uid uint32) (string, bool) {
	n, ok := p.byUID[uid]
	return n, ok
}

// Exists reports whether a local user with this name exists.
func (p *Passwd) Exists(name string) bool {
	_, ok := p.byName[name]
	return ok
}

// SubIDRange is one /etc/subuid entry.
type SubIDRange struct {
	Owner    string // username, or "" when the line uses the uid form
	OwnerUID uint32 // set when the line uses the uid form
	ByUID    bool
	Start    uint64
	Count    uint64
}

// Contains reports whether uid is inside the range.
func (r SubIDRange) Contains(uid uint32) bool {
	return uint64(uid) >= r.Start && uint64(uid) < r.Start+r.Count
}

// ParseSubuid parses subuid(5) content. Both "name:start:count" and
// "uid:start:count" forms are accepted. Malformed lines are skipped and
// returned as English warnings.
func ParseSubuid(data []byte) ([]SubIDRange, []string) {
	var out []SubIDRange
	var warnings []string
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		bad := func(why string) {
			warnings = append(warnings, fmt.Sprintf("subuid: skipping malformed line %d: %s", i+1, why))
		}
		if len(f) != 3 || f[0] == "" {
			bad("expected three fields name:start:count")
			continue
		}
		start, err1 := strconv.ParseUint(f[1], 10, 32)
		count, err2 := strconv.ParseUint(f[2], 10, 32)
		if err1 != nil || err2 != nil || count == 0 {
			bad("start and count must be positive integers")
			continue
		}
		r := SubIDRange{Start: start, Count: count}
		if uid, err := strconv.ParseUint(f[0], 10, 32); err == nil {
			r.ByUID, r.OwnerUID = true, uint32(uid)
		} else {
			r.Owner = f[0]
		}
		out = append(out, r)
	}
	return out, warnings
}

// ParseLoginDefsUIDMin returns UID_MIN from login.defs(5) content.
func ParseLoginDefsUIDMin(data []byte) (uint32, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "UID_MIN" {
			if v, err := strconv.ParseUint(f[1], 10, 32); err == nil {
				return uint32(v), true
			}
		}
	}
	return 0, false
}

// Accounts is the account database of one host.
type Accounts struct {
	Passwd *Passwd
	Subuid []SubIDRange
	UIDMin uint32
}

// Load reads the account files through fs. Problems are returned as English
// messages: errs for unavailable sources, warnings for skipped lines. A
// missing source never prevents the rest from loading.
func Load(fs hostfs.FS, uidMinOverride *uint32) (acc *Accounts, errs, warnings []string) {
	acc = &Accounts{Passwd: ParsePasswd(nil), UIDMin: DefaultUIDMin}
	if b, err := fs.ReadFile("/etc/passwd"); err != nil {
		errs = append(errs, fmt.Sprintf("passwd: cannot read /etc/passwd: %v", err))
	} else {
		acc.Passwd = ParsePasswd(b)
	}
	if b, err := fs.ReadFile("/etc/subuid"); err != nil {
		errs = append(errs, fmt.Sprintf("subuid: cannot read /etc/subuid: %v", err))
	} else {
		acc.Subuid, warnings = ParseSubuid(b)
	}
	switch {
	case uidMinOverride != nil:
		acc.UIDMin = *uidMinOverride
	default:
		if b, err := fs.ReadFile("/etc/login.defs"); err == nil {
			if v, ok := ParseLoginDefsUIDMin(b); ok {
				acc.UIDMin = v
			}
		}
	}
	return acc, errs, warnings
}

// Username returns the passwd name of uid, or "uid:<n>" when there is none.
func (a *Accounts) Username(uid uint32) string {
	if n, ok := a.Passwd.Name(uid); ok {
		return n
	}
	return fmt.Sprintf("uid:%d", uid)
}

// SubuidOwner returns the owner of the subuid range containing uid.
func (a *Accounts) SubuidOwner(uid uint32) (owner string, r SubIDRange, ok bool) {
	for _, r := range a.Subuid {
		if r.Contains(uid) {
			if r.ByUID {
				return a.Username(r.OwnerUID), r, true
			}
			return r.Owner, r, true
		}
	}
	return "", SubIDRange{}, false
}

// FileOwner maps a file UID to an owner for the disk scan: subuid range
// owner first, then the passwd name, then "uid:<n>".
func (a *Accounts) FileOwner(uid uint32) string {
	if o, _, ok := a.SubuidOwner(uid); ok {
		return o
	}
	return a.Username(uid)
}
