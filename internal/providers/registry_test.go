// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package providers

import (
	"context"
	"testing"
	"time"
)

// fakeSearchProvider is a BookSearchProvider whose SearchBooks call blocks
// for `delay` before returning a single result named after the provider.
type fakeSearchProvider struct {
	name  string
	delay time.Duration
}

func (p *fakeSearchProvider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, DisplayName: p.name, Capabilities: []string{CapBookSearch}}
}
func (p *fakeSearchProvider) Configure(map[string]string) {}
func (p *fakeSearchProvider) Enabled() bool               { return true }
func (p *fakeSearchProvider) SearchBooks(ctx context.Context, query string) ([]*BookResult, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []*BookResult{{Provider: p.name, Title: p.name}}, nil
}

// withSearchDeadline temporarily shrinks the package-level deadline so tests
// don't have to sleep several real seconds per case, restoring it after.
func withSearchDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	orig := searchDeadline
	searchDeadline = d
	t.Cleanup(func() { searchDeadline = orig })
}

// Regression test for the bug found live 2026-07-26: the deadline timer was
// started unconditionally at the top of SearchBooks, so if the *fastest*
// provider itself took longer than searchDeadline, the deadline could fire
// before any result at all arrived — the opposite of the doc comment's
// intent ("waits for lagging providers once at least one provider has
// already returned results").
func TestSearchBooks_DeadlineStartsAfterFirstResult_NotAtSearchStart(t *testing.T) {
	withSearchDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	// Both providers are individually slower than searchDeadline on their
	// own — under the old flat-from-start timer, the deadline would fire
	// at 50ms, before either had a chance to respond, yielding zero results.
	r.Register(&fakeSearchProvider{name: "slow-first", delay: 60 * time.Millisecond})
	r.Register(&fakeSearchProvider{name: "slow-second", delay: 100 * time.Millisecond})

	start := time.Now()
	out := r.SearchBooks(context.Background(), "query")
	elapsed := time.Since(start)

	if len(out) != 2 {
		t.Fatalf("got %d results, want 2 (both providers, since the deadline should only start counting after the first response) — results: %+v", len(out), out)
	}
	// Sanity bound: should finish well before the two providers' delays
	// summed, proving they ran concurrently rather than serially.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("took %s, expected well under 200ms for two concurrent providers", elapsed)
	}
}

func TestSearchBooks_DropsStragglerPastGracePeriodAfterFirstResult(t *testing.T) {
	withSearchDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	r.Register(&fakeSearchProvider{name: "fast", delay: 0})
	// Arrives 150ms after the fast provider — well past fast's 0+50ms grace
	// window — so it must be dropped even though the fix removed the flat
	// from-search-start timer.
	r.Register(&fakeSearchProvider{name: "straggler", delay: 150 * time.Millisecond})

	out := r.SearchBooks(context.Background(), "query")

	if len(out) != 1 || out[0].Provider != "fast" {
		t.Fatalf("got %+v, want exactly the fast provider's result — the straggler should have been dropped", out)
	}
}

// fakeISBNProvider is a BookISBNProvider whose lookup blocks for `delay`
// before returning a single result named after the provider.
type fakeISBNProvider struct {
	name  string
	delay time.Duration
}

func (p *fakeISBNProvider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, DisplayName: p.name, Capabilities: []string{CapBookISBN}}
}
func (p *fakeISBNProvider) Configure(map[string]string) {}
func (p *fakeISBNProvider) Enabled() bool               { return true }
func (p *fakeISBNProvider) LookupByISBN(ctx context.Context, isbn string) (*BookResult, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &BookResult{Provider: p.name, Title: p.name}, nil
}

func withISBNDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	orig := isbnDeadline
	isbnDeadline = d
	t.Cleanup(func() { isbnDeadline = orig })
}

// Regression test for the bug reported 2026-08-17: LookupISBN waited for every
// provider with no aggregate deadline, so one unreachable provider cost every
// scan its full HTTP timeout even when another had already returned the book.
// Reported as scan timeouts on iOS and web during an Open Library outage,
// where Open Library hung rather than refusing.
func TestLookupISBN_DoesNotWaitForAHungProviderOnceOneAnswered(t *testing.T) {
	withISBNDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	r.Register(&fakeISBNProvider{name: "fast", delay: 0})
	// Stands in for a provider that hangs until its own HTTP timeout.
	r.Register(&fakeISBNProvider{name: "hung", delay: 10 * time.Second})

	start := time.Now()
	out := r.LookupISBN(context.Background(), "9780441172719")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("waited %v for a hung provider; the scan should return once a fast one answered", elapsed)
	}
	if len(out) != 1 || out[0].Provider != "fast" {
		t.Fatalf("want the fast provider's result, got %+v", out)
	}
}

// The deadline must not start before anything has answered, or a lookup where
// every provider is merely slow returns nothing at all.
func TestLookupISBN_DeadlineStartsAfterFirstResult(t *testing.T) {
	withISBNDeadline(t, 50*time.Millisecond)

	r := NewRegistry()
	r.Register(&fakeISBNProvider{name: "slow", delay: 200 * time.Millisecond})

	out := r.LookupISBN(context.Background(), "9780441172719")
	if len(out) != 1 {
		t.Fatalf("want the slow provider's result once it arrives, got %d results", len(out))
	}
}

// A background enrichment has nobody waiting, so it must keep the answer of a
// provider slower than the scan deadline rather than dropping it.
func TestLookupISBN_UnhurriedWaitsForSlowProviders(t *testing.T) {
	withISBNDeadline(t, 50*time.Millisecond)
	orig := lookupOverallDeadline
	lookupOverallDeadline = 100 * time.Millisecond
	t.Cleanup(func() { lookupOverallDeadline = orig })

	r := NewRegistry()
	r.Register(&fakeISBNProvider{name: "fast", delay: 0})
	r.Register(&fakeISBNProvider{name: "paced", delay: 300 * time.Millisecond})

	out := r.LookupISBN(Unhurried(context.Background()), "9780441172719")
	if len(out) != 2 {
		t.Fatalf("want both providers' results, got %+v", out)
	}
}

// fakeUPCProvider answers UPC lookups and records the code it was asked for.
type fakeUPCProvider struct {
	name  string
	asked string
}

func (p *fakeUPCProvider) Info() ProviderInfo {
	return ProviderInfo{Name: p.name, DisplayName: p.name, Capabilities: []string{CapBookUPC}}
}
func (p *fakeUPCProvider) Configure(map[string]string) {}
func (p *fakeUPCProvider) Enabled() bool               { return true }
func (p *fakeUPCProvider) LookupByUPC(_ context.Context, code string) (*BookResult, error) {
	p.asked = code
	return &BookResult{Provider: p.name, Title: p.name}, nil
}

// A UPC goes only to providers that claim book_upc; an ISBN-only provider
// would take the code for a malformed ISBN and waste a request.
func TestLookupUPC_AsksOnlyUPCProviders(t *testing.T) {
	r := NewRegistry()
	upc := &fakeUPCProvider{name: "upc"}
	r.Register(upc)
	r.Register(&fakeISBNProvider{name: "isbn-only"})

	out := r.LookupUPC(context.Background(), "036000291452")
	if len(out) != 1 || out[0].Provider != "upc" {
		t.Fatalf("want only the UPC provider's result, got %+v", out)
	}
	if upc.asked != "036000291452" {
		t.Fatalf("UPC provider was asked for %q", upc.asked)
	}
}

func TestLookupUPC_NoProvidersReturnsNil(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeISBNProvider{name: "isbn-only"})
	if out := r.LookupUPC(context.Background(), "036000291452"); out != nil {
		t.Fatalf("want nil with no UPC providers, got %+v", out)
	}
}

// The merged UPC lookup needs to say who answered, the same as an ISBN one.
func TestLookupUPCReport_SaysWhoAnswered(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeUPCProvider{name: "upc"})
	r.Register(&fakeISBNProvider{name: "isbn-only"})

	out, statuses := r.LookupUPCReport(context.Background(), "036000291452")
	if len(out) != 1 || len(statuses) != 1 || statuses[0].Name != "upc" {
		t.Fatalf("results %+v, statuses %+v", out, statuses)
	}
}
