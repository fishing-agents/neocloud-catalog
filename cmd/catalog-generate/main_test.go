// SPDX-FileCopyrightText: 2026 Reza Jelveh
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
	if err := os.CopyFS(filepath.Join(dir, "data"), os.DirFS("../../data")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots/v4"), 0o755); err != nil {
		t.Fatal(err)
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

func TestRunRefusesUnknownTemplateField(t *testing.T) {
	inDataDir(t)
	path := "data/templates/platform-kagent-0.10.4.json"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(`"app_api_groups"`), []byte(`"app_api_group"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(); err == nil {
		t.Fatal("run accepted a misspelt template field")
	}
}
