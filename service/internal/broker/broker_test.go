// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package broker_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fishman/neocloud-catalog/service/internal/broker"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface"
	"github.com/fishman/neocloud-catalog/service/internal/importers/huggingface/hubtest"
)

// A search answers from the cache until SearchTTL; upstream searches are rate limited, cached ones
// are not; modelscope is refused.
func TestSearchCacheAndRateLimit(t *testing.T) {
	ctx := context.Background()
	h := hubtest.New(t, nil)
	b := broker.New(nil, huggingface.New(h.URL, "operator-token"))
	now := time.Now()
	b.SetNow(func() time.Time { return now })

	for range 3 {
		hits, err := b.Search(ctx, "huggingface", "qwen", 1)
		if err != nil || len(hits) != 1 || hits[0].Repo != "Qwen/Qwen2.5-14B-Instruct" || hits[0].Downloads != 1346684 {
			t.Fatalf("search = %+v, %v", hits, err)
		}
	}
	if n := h.Searches(); n != 1 {
		t.Errorf("3 equal searches went upstream %d times, want 1", n)
	}
	now = now.Add(broker.SearchTTL)
	if _, err := b.Search(ctx, "huggingface", "qwen", 1); err != nil || h.Searches() != 2 {
		t.Errorf("search after the TTL: %v, %d upstream searches, want 2", err, h.Searches())
	}
	for _, auth := range h.Authorizations() {
		if auth != "" {
			t.Errorf("search sent Authorization %q; searches are anonymous", auth)
		}
	}

	var limited error
	for i := 0; limited == nil && i < 100; i++ {
		_, limited = b.Search(ctx, "huggingface", fmt.Sprint("q", i), 2)
	}
	if !errors.Is(limited, broker.ErrRateLimited) {
		t.Fatalf("100 distinct searches: %v, want rate limited", limited)
	}
	if _, err := b.Search(ctx, "huggingface", "qwen", 1); err != nil {
		t.Errorf("a cached search while rate limited: %v", err)
	}
	if _, err := b.Search(ctx, "modelscope", "qwen", 1); !errors.Is(err, broker.ErrUnsupported) {
		t.Errorf("modelscope search: %v, want unsupported", err)
	}
}
