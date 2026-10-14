// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var nodeTypeID = regexp.MustCompile(`^[a-z]+/[A-Za-z0-9._-]+$`)

// fallbackProvider is reserved for generated fallbacks, so the fallback set stays
// exactly family x {1,2,4,8}.
const fallbackProvider = "other/"

// FallbackNodeType is the id for a system that is not a listed cloud instance.
func FallbackNodeType(family string, gpus int) string {
	return fmt.Sprintf("%s%sx%d", fallbackProvider, family, gpus)
}

// NodeTypeVocabulary validates the cloud instances and adds 1, 2, 4 and 8 GPU
// fallbacks per family, sorted by id.
func NodeTypeVocabulary(families []Family, cloud []NodeType) ([]NodeType, error) {
	known := make(map[string]bool, len(families))
	for _, f := range families {
		known[f.Name] = true
	}
	var errs []error
	for _, nt := range cloud {
		switch {
		case !nodeTypeID.MatchString(nt.ID):
			errs = append(errs, fmt.Errorf("node type %q: invalid id", nt.ID))
		case strings.HasPrefix(nt.ID, fallbackProvider):
			errs = append(errs, fmt.Errorf("node type %q: provider %q is reserved for generated fallbacks", nt.ID, fallbackProvider))
		}
		if !known[nt.Family] {
			errs = append(errs, fmt.Errorf("node type %q: unknown family %q", nt.ID, nt.Family))
		}
		if nt.GPUs < 1 {
			errs = append(errs, fmt.Errorf("node type %q: gpus %d < 1", nt.ID, nt.GPUs))
		}
		if !strings.HasPrefix(nt.Source, "https://") {
			errs = append(errs, fmt.Errorf("node type %q: source %q is not an https URL", nt.ID, nt.Source))
		}
	}

	vocab := make([]NodeType, 0, len(cloud)+4*len(families))
	vocab = append(vocab, cloud...)
	for _, f := range families {
		for _, n := range []int{1, 2, 4, 8} {
			id := FallbackNodeType(f.Name, n)
			if !nodeTypeID.MatchString(id) {
				errs = append(errs, fmt.Errorf("family %q: fallback id %q is invalid", f.Name, id))
			}
			vocab = append(vocab, NodeType{ID: id, Family: f.Name, GPUs: n})
		}
	}
	slices.SortFunc(vocab, func(a, b NodeType) int { return strings.Compare(a.ID, b.ID) })
	for i := 1; i < len(vocab); i++ {
		if vocab[i].ID == vocab[i-1].ID {
			errs = append(errs, fmt.Errorf("node type %q: duplicate id", vocab[i].ID))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return vocab, nil
}
