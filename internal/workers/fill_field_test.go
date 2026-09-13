// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package workers

import "testing"

func TestFillField(t *testing.T) {
	cases := []struct {
		name            string
		current, merged string
		force           bool
		want            string
	}{
		{"fill empty from provider", "", "Real Title", false, "Real Title"},
		{"keep existing when not forcing", "Existing", "Provider", false, "Existing"},
		{"force overwrites with a real value", "Existing", "Provider", true, "Provider"},
		// The regression: a force re-enrich whose lookup came back empty must
		// NOT blank out an existing value. This is what wiped 33 books.
		{"force does NOT wipe with empty", "Existing", "", true, "Existing"},
		{"empty stays empty when provider has nothing", "", "", true, ""},
		{"non-force empty current, empty provider", "", "", false, ""},
	}
	for _, tc := range cases {
		if got := fillField(tc.current, tc.merged, tc.force); got != tc.want {
			t.Errorf("%s: fillField(%q,%q,%v) = %q, want %q",
				tc.name, tc.current, tc.merged, tc.force, got, tc.want)
		}
	}
}
