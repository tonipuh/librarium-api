// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fireball1725/librarium-api/internal/providers"
)

func TestNormalizeBookResultSlices(t *testing.T) {
	results := []*providers.BookResult{
		// A provider hit with no authors/categories — Go leaves these nil, which
		// marshals to `null`. That's what broke the iOS scan on a Google Books
		// result with no authors.
		{Provider: "google_books", Title: "Hiiop, Jeeves"},
		{Provider: "finna", Title: "Hiiop, Jeeves", Authors: []string{"P. G. Wodehouse"}, Categories: []string{"huumori"}},
	}

	normalizeBookResultSlices(results)

	if results[0].Authors == nil || results[0].Categories == nil {
		t.Fatal("nil slices were not normalised to non-nil")
	}
	// Existing values must survive.
	if len(results[1].Authors) != 1 || results[1].Authors[0] != "P. G. Wodehouse" {
		t.Errorf("existing authors changed: %#v", results[1].Authors)
	}

	// The contract clients rely on: never `null` for these arrays.
	blob, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), `"authors":null`) || strings.Contains(string(blob), `"categories":null`) {
		t.Errorf("response still contains a null array: %s", blob)
	}
}
