package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Verifies: REQ-SEC-001
func TestAgentSourceOpensNoListeningSockets(t *testing.T) {
	root := filepath.Join("..", "..")
	forbidden := regexp.MustCompile(`\bnet\.Listen\w*\(|\bhttp\.ListenAndServe\w*\(|\.ListenAndServe\w*\(|\bnet\.FileListener\(|\bhttp\.Serve\(|\bsyscall\.Listen\(`)
	checked := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && (info.Name() == "testdata" || info.Name() == "vendor") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		checked++
		if m := forbidden.Find(b); m != nil {
			t.Errorf("%s calls %s; the agent must not listen", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 10 {
		t.Fatalf("only %d source files checked", checked)
	}
}

// Verifies: REQ-AGT-013
func TestDiagnoseIsAFirstClassCommand(t *testing.T) {
	if !strings.Contains(usage, "diagnose") || !strings.Contains(usage, "simulate") {
		t.Fatal("usage must list diagnose and simulate")
	}
}
