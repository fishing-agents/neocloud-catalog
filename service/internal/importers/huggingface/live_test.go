package huggingface

import (
	"os"
	"reflect"
	"strings"
	"testing"

	catalog "github.com/fishman/neocloud-catalog"
	"github.com/fishman/neocloud-catalog/service/internal/store"
)

// Live Hub matches testdata/hf, read without a token. Opt in with
// NEOCLOUD_CATALOG_LIVE=1.
func TestLiveHubMatchesRecordings(t *testing.T) {
	if os.Getenv("NEOCLOUD_CATALOG_LIVE") != "1" {
		t.Skip("NEOCLOUD_CATALOG_LIVE unset; skipping the live Hugging Face test")
	}
	recorded := &client{Importer: testImporter(hub(t).URL, "")}
	live := &client{Importer: New(Endpoint, "")}
	jobs := append(plan(committed(t), func(string, string) bool { return true }), job{owner: store.Manifest{ModelSlug: "missing"},
		src: catalog.ModelSource{Type: catalog.SourceHuggingFace, Repo: "Qwen/Qwen2.5-14B-Instruct", Revision: strings.Repeat("f", 40)}})
	for _, j := range jobs {
		if got, want := live.read(ctx, j).record(), recorded.read(ctx, j).record(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n live     %+v\n recorded %+v", j, got, want)
		}
	}
}
