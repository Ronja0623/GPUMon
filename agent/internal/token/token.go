// Package token reads the shared bearer token. The token value is never logged.
package token

import (
	"fmt"
	"os"
	"strings"
)

// Read returns the token stored in path. It refuses files whose permissions
// are wider than 0600.
func Read(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read token file %s: %v", path, err)
	}
	if perm := st.Mode().Perm(); perm&^0o600 != 0 {
		return "", fmt.Errorf("token file %s has permissions %04o; refusing to start (must be 0600 or stricter)", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read token file %s: %v", path, err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return t, nil
}
