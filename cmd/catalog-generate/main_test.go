// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
)

// inDataDir changes to a temp dir with a copy of data/ and an empty snapshots/v4.
func inDataDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots/v4"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"models.json", "draft_models.json", "families.json", "backends.json", "node_types.json"} {
		b, err := os.ReadFile(filepath.Join("../../data", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestRunKeepsVersion(t *testing.T) {
	inDataDir(t)
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

// A previous snapshot of an older shape is changed content, not an error.
func TestRunReplacesUndecodablePrevious(t *testing.T) {
	inDataDir(t)
	const old = "2026-01-01T00:00:00Z"
	prev := `{"schema_version":"4.0.0","version":"` + old + `","backends":[{"name":"vllm","versions":[{"flags":{"chat_templates":{"llama3.1-tools":"/x"}}}]}]}`
	if err := os.WriteFile(snapshotPath, []byte(prev), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap catalog.CatalogSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ParseVersion(snap.Version); err != nil || snap.Version == old {
		t.Errorf("version %q (%v), want a new generation version", snap.Version, err)
	}
}

func TestRunFailsOnUnreadablePrevious(t *testing.T) {
	inDataDir(t)
	if err := os.Mkdir(snapshotPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("run succeeded with a directory as the previous snapshot")
	}
}
