package catalog

import (
	"os/exec"
	"strings"
	"testing"
)

// The public core must stay stdlib-only: enterprise code plugs in through
// extensions, never through imports.
func TestCoreImportsOnlyStdlib(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".", "./runrecord", "./cmd/...").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range strings.Fields(string(out)) {
		if !strings.HasPrefix(p, "github.com/fishman/neocloud-catalog") {
			t.Errorf("core depends on %s", p)
		}
	}
}
