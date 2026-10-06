package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots/v3"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"models.json", "families.json", "backends.json"} {
		b, err := os.ReadFile(filepath.Join("../../data", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)

	if err := run(); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("regenerating unchanged data rewrote the snapshot")
	}
}
