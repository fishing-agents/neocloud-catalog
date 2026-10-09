//go:build atlas

// Command atlas dumps the GORM models as DDL for atlas-provider-gorm: the
// external schema atlas.hcl feeds `atlas migrate diff`. Built only with the
// atlas tag, never into catalog-service.
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
