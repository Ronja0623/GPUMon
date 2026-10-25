package token_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpumon/agent/internal/token"
)

func TestREQ_SEC_002_TokenFilePermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	const secret = "s3cr3t-token-value"
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		got, err := token.Read(p)
		if err != nil || got != secret {
			t.Fatalf("mode %04o: got %q, %v", mode, got, err)
		}
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o700, 0o666} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		_, err := token.Read(p)
		if err == nil || !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("mode %04o must be refused, got %v", mode, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("the error leaks the token")
		}
	}
	if _, err := token.Read(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing token file must be an error")
	}
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := token.Read(empty); err == nil {
		t.Fatal("an empty token must be an error")
	}
}
