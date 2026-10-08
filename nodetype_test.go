package catalog

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestNodeTypesVocabulary(t *testing.T) {
	var families []Family
	var cloud []NodeType
	for path, v := range map[string]any{"data/families.json": &families, "data/node_types.json": &cloud} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	vocab, err := NodeTypeVocabulary(families, cloud)
	if err != nil {
		t.Fatal(err)
	}
	if got := FallbackNodeType("H100", 8); got != "other/H100x8" {
		t.Errorf("FallbackNodeType(H100, 8) = %q", got)
	}

	idRE := regexp.MustCompile(`^[a-z]+/[A-Za-z0-9._-]+$`)
	known := map[string]bool{}
	for _, f := range families {
		known[f.Name] = true
	}
	byID := map[string]NodeType{}
	for _, nt := range vocab {
		if !idRE.MatchString(nt.ID) {
			t.Errorf("%s: id does not match %s", nt.ID, idRE)
		}
		if _, dup := byID[nt.ID]; dup {
			t.Errorf("%s: duplicate id", nt.ID)
		}
		byID[nt.ID] = nt
		if !known[nt.Family] {
			t.Errorf("%s: unknown family %q", nt.ID, nt.Family)
		}
		if nt.GPUs < 1 {
			t.Errorf("%s: gpus = %d", nt.ID, nt.GPUs)
		}
		if !strings.HasPrefix(nt.ID, "other/") && !strings.HasPrefix(nt.Source, "https://") {
			t.Errorf("%s: cloud instance without an https source", nt.ID)
		}
	}
	if !slices.IsSortedFunc(vocab, func(a, b NodeType) int { return strings.Compare(a.ID, b.ID) }) {
		t.Error("vocabulary not sorted by id")
	}
	for _, c := range cloud {
		if byID[c.ID] != c {
			t.Errorf("%s: curated entry missing or altered", c.ID)
		}
	}
	for _, f := range families {
		for _, n := range []int{1, 2, 4, 8} {
			id := FallbackNodeType(f.Name, n)
			if nt, ok := byID[id]; !ok || nt.Family != f.Name || nt.GPUs != n {
				t.Errorf("fallback %s: got %+v", id, nt)
			}
		}
	}
}

func TestNodeTypeVocabularyRejectsInvalid(t *testing.T) {
	families := []Family{{Name: "H100"}}
	ok := NodeType{ID: "aws/p5.48xlarge", Family: "H100", GPUs: 8, Source: "https://aws.amazon.com/ec2/instance-types/p5/"}
	cases := []struct {
		name string
		edit func(*NodeType)
		want string
	}{
		{"valid", func(*NodeType) {}, ""},
		{"bad id", func(n *NodeType) { n.ID = "AWS/p5 48xlarge" }, "invalid id"},
		{"curated fallback", func(n *NodeType) { n.ID = "other/H100x3" }, "reserved"},
		{"unknown family", func(n *NodeType) { n.Family = "B200" }, "unknown family"},
		{"no gpus", func(n *NodeType) { n.GPUs = 0 }, "gpus"},
		{"http source", func(n *NodeType) { n.Source = "http://aws.amazon.com/" }, "https"},
	}
	for _, c := range cases {
		nt := ok
		c.edit(&nt)
		_, err := NodeTypeVocabulary(families, []NodeType{nt})
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	if _, err := NodeTypeVocabulary(families, []NodeType{ok, ok}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: err = %v", err)
	}
}
