package model

import (
	"slices"
	"testing"
)

func TestCuratedFieldsOnlyCurationSourced(t *testing.T) {
	p := FieldProvenance{
		"params_b":     {Source: "huggingface", SourceURL: "https://huggingface.co/m"},
		"description":  {Source: SourceCuration},
		"context_max":  {Source: SourceCuration},
		"architecture": {Source: "huggingface"},
	}
	if got, want := p.CuratedFields(), []string{"context_max", "description"}; !slices.Equal(got, want) {
		t.Errorf("CuratedFields = %v, want %v", got, want)
	}
	if got := FieldProvenance(nil).CuratedFields(); len(got) != 0 {
		t.Errorf("CuratedFields of no provenance = %v, want none", got)
	}
}
