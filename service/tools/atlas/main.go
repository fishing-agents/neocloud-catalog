// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

//go:build atlas

// Command atlas dumps the GORM models as DDL, the schema atlas.hcl diffs
// against. Built only with the atlas tag.
package main

import (
	"fmt"
	"io"
	"os"

	"ariga.io/atlas-provider-gorm/gormschema"

	"github.com/fishman/neocloud-catalog/service/internal/model"
)

func main() {
	stmts, err := gormschema.New("postgres").Load(model.All()...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dump schema:", err)
		os.Exit(1)
	}
	if _, err := io.WriteString(os.Stdout, stmts); err != nil {
		fmt.Fprintln(os.Stderr, "write schema:", err)
		os.Exit(1)
	}
}
