// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package providers

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Registry holds all registered metadata providers.
type Registry struct {
	mu        sync.RWMutex
	providers []MetadataProvider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a provider to the registry.
func (r *Registry) Register(p MetadataProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = append(r.providers, p)
}

// All returns all registered providers (enabled or not).
func (r *Registry) All() []MetadataProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]MetadataProvider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Configure updates a single provider's config by name.
func (r *Registry) Configure(name string, cfg map[string]string) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if p.Info().Name == name {
			p.Configure(cfg)
			return
		}
	}
}

// BookISBNProviders returns all enabled providers with the book_isbn capability.
func (r *Registry) BookISBNProviders() []BookISBNProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []BookISBNProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if bp, ok := p.(BookISBNProvider); ok {
			out = append(out, bp)
		}
	}
	return out
}

// BookUPCProviders returns all enabled providers with the book_upc capability.
func (r *Registry) BookUPCProviders() []BookUPCProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []BookUPCProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if up, ok := p.(BookUPCProvider); ok {
			out = append(out, up)
		}
	}
	return out
}

// SeriesSearchProviders returns all enabled providers with the series_name capability.
func (r *Registry) SeriesSearchProviders() []SeriesSearchProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []SeriesSearchProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if sp, ok := p.(SeriesSearchProvider); ok {
			out = append(out, sp)
		}
	}
	return out
}

// LookupISBN queries all enabled BookISBNProviders concurrently and returns
// all non-nil results. Errors from individual providers are silently skipped.
func (r *Registry) LookupISBN(ctx context.Context, isbn string) []*BookResult {
	results, _ := r.LookupISBNReport(ctx, isbn)
	return results
}

// LookupISBNReport is LookupISBN plus what happened to each provider, so the
// client can say who answered and who didn't.
func (r *Registry) LookupISBNReport(ctx context.Context, isbn string) ([]*BookResult, []ProviderStatus) {
	var lookups []barcodeLookup
	for _, p := range r.BookISBNProviders() {
		info := p.Info()
		lookups = append(lookups, barcodeLookup{name: info.Name, display: info.DisplayName, fn: p.LookupByISBN})
	}
	return lookupBarcode(ctx, "isbn", isbn, lookups)
}

// LookupUPC queries all enabled BookUPCProviders the same way LookupISBN does.
func (r *Registry) LookupUPC(ctx context.Context, code string) []*BookResult {
	results, _ := r.LookupUPCReport(ctx, code)
	return results
}

// LookupUPCReport is LookupUPC plus what happened to each provider.
func (r *Registry) LookupUPCReport(ctx context.Context, code string) ([]*BookResult, []ProviderStatus) {
	var lookups []barcodeLookup
	for _, p := range r.BookUPCProviders() {
		info := p.Info()
		lookups = append(lookups, barcodeLookup{name: info.Name, display: info.DisplayName, fn: p.LookupByUPC})
	}
	return lookupBarcode(ctx, "upc", code, lookups)
}

// LookupOne asks a single enabled provider about an ISBN, for when a
// provider is turned on after a book was added. ok is false when there's no
// enabled ISBN provider by that name.
func (r *Registry) LookupOne(ctx context.Context, name, isbn string) (book *BookResult, ok bool, err error) {
	for _, p := range r.BookISBNProviders() {
		if p.Info().Name == name {
			book, err = p.LookupByISBN(ctx, isbn)
			return book, true, err
		}
	}
	return nil, false, nil
}

// Provider outcomes in a lookup report.
const (
	StatusAnswered = "answered"
	StatusNoRecord = "no_record"
	StatusError    = "error"
	StatusMissed   = "missed" // didn't answer before the deadline
)

// ProviderStatus is what one provider did during a lookup.
type ProviderStatus struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	// Millis is how long it took to answer; 0 when it missed the deadline.
	Millis int64 `json:"millis"`
}

type barcodeLookup struct {
	name    string
	display string
	fn      func(context.Context, string) (*BookResult, error)
}

// lookupBarcode asks every provider for one barcode at once and returns what
// came back, in the order it arrived, plus a status per provider in the order
// they were asked. kind only labels the log lines.
func lookupBarcode(ctx context.Context, kind, code string, lookups []barcodeLookup) ([]*BookResult, []ProviderStatus) {
	if len(lookups) == 0 {
		return nil, nil
	}

	type result struct {
		i      int
		book   *BookResult
		status string
		millis int64
	}

	statuses := make([]ProviderStatus, len(lookups))
	for i, l := range lookups {
		statuses[i] = ProviderStatus{Name: l.name, DisplayName: l.display, Status: StatusMissed}
	}

	start := time.Now()
	ch := make(chan result, len(lookups))
	for i, l := range lookups {
		go func(i int, l barcodeLookup) {
			book, err := l.fn(ctx, code)
			ms := time.Since(start).Milliseconds()
			switch {
			case err != nil:
				slog.WarnContext(ctx, kind+" lookup provider error", "provider", l.name, kind, code, "error", err)
				ch <- result{i: i, status: StatusError, millis: ms}
			case book == nil:
				ch <- result{i: i, status: StatusNoRecord, millis: ms}
			default:
				ch <- result{i: i, book: book, status: StatusAnswered, millis: ms}
			}
		}(i, l)
	}

	// Same shape as SearchBooks: once one provider has answered, the rest get
	// isbnDeadline and then we return what we have.
	//
	// Without this the lookup took as long as the slowest provider, so a
	// single unreachable one cost every scan its full HTTP timeout even when
	// another provider had already returned the book. Reported by a user on
	// 2026-08-17 as scan timeouts on both iOS and web during an Open Library
	// outage; Open Library was hanging rather than refusing, so it burned its
	// whole 15s on every scan.
	//
	// lookupOverallDeadline caps the whole lookup as well, so a scan where
	// every provider is slow still comes back instead of waiting on HTTP
	// timeouts.
	//
	// Neither applies to an Unhurried lookup: it waits for every provider,
	// each still bounded by its own HTTP timeout.
	unhurried := isUnhurried(ctx)
	var deadlineC, overallC <-chan time.Time
	if !unhurried {
		overall := time.NewTimer(lookupOverallDeadline)
		defer overall.Stop()
		overallC = overall.C
	}

	var out []*BookResult
	remaining := len(lookups)
	for remaining > 0 {
		select {
		case res := <-ch:
			statuses[res.i].Status = res.status
			statuses[res.i].Millis = res.millis
			if res.book != nil {
				out = append(out, res.book)
			}
			remaining--
			if deadlineC == nil && !unhurried {
				deadline := time.NewTimer(isbnDeadline)
				defer deadline.Stop()
				deadlineC = deadline.C
			}
		case <-deadlineC:
			slog.InfoContext(ctx, kind+" lookup deadline reached, returning partial results",
				kind, code, "waiting_on", remaining, "results_so_far", len(out))
			return out, statuses
		case <-overallC:
			slog.InfoContext(ctx, kind+" lookup overall deadline reached",
				kind, code, "waiting_on", remaining, "results_so_far", len(out))
			return out, statuses
		case <-ctx.Done():
			return out, statuses
		}
	}
	return out, statuses
}

// lookupOverallDeadline bounds a whole lookup, including the wait for the
// first answer. var so tests can shrink it.
var lookupOverallDeadline = 10 * time.Second

// isbnDeadline is how long LookupISBN waits for lagging providers once at
// least one has answered.
//
// Tighter than searchDeadline because the two are not the same act. A search
// produces a list to browse, and a wider net is worth a wait. An ISBN lookup
// is someone standing at a shelf with a book in their hand and a scanner
// pointed at it, where a second provider's extra fields are worth much less
// than answering promptly.
//
// var, not const, so tests can shrink it rather than sleeping real seconds.
var isbnDeadline = 3 * time.Second

type unhurriedKey struct{}

// Unhurried marks ctx as a lookup nobody is waiting on, such as a background
// enrichment batch, so ISBN and UPC lookups wait for every provider rather
// than cutting the slow ones off at isbnDeadline.
//
// The deadlines are for a scan at the shelf. In a batch they drop exactly the
// providers that pace themselves: several batches share Finna's one request a
// second, its queue passes three seconds at once, and Finna then never
// contributed to a batch at all.
func Unhurried(ctx context.Context) context.Context {
	return context.WithValue(ctx, unhurriedKey{}, true)
}

func isUnhurried(ctx context.Context) bool {
	v, _ := ctx.Value(unhurriedKey{}).(bool)
	return v
}

// BookSearchProviders returns all enabled providers with the book_search capability.
func (r *Registry) BookSearchProviders() []BookSearchProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []BookSearchProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if bp, ok := p.(BookSearchProvider); ok {
			out = append(out, bp)
		}
	}
	return out
}

// searchDeadline is how long SearchBooks waits for lagging providers once at
// least one provider has already returned results.  A slow provider (e.g. Open
// Library search timing out at 15 s) will not hold up results from fast ones.
// var, not const, so tests can shrink it rather than sleeping several real
// seconds per case.
var searchDeadline = 5 * time.Second

// SearchBooks queries all enabled BookSearchProviders concurrently and returns
// as soon as every provider has responded OR the deadline is reached.
func (r *Registry) SearchBooks(ctx context.Context, query string) []*BookResult {
	providers := r.BookSearchProviders()
	slog.InfoContext(ctx, "book search start", "query", query, "providers", len(providers))
	if len(providers) == 0 {
		return nil
	}

	type result struct {
		name  string
		items []*BookResult
	}

	ch := make(chan result, len(providers))
	for _, p := range providers {
		go func(bp BookSearchProvider) {
			name := bp.Info().Name
			items, err := bp.SearchBooks(ctx, query)
			if err != nil {
				slog.WarnContext(ctx, "book search provider error", "provider", name, "error", err)
				ch <- result{name: name}
				return
			}
			slog.InfoContext(ctx, "book search provider ok", "provider", name, "results", len(items))
			ch <- result{name: name, items: items}
		}(p)
	}

	// deadlineC stays nil (blocks forever in the select below) until the
	// first result arrives, matching searchDeadline's doc comment: lagging
	// providers get searchDeadline *after* a fast one has already
	// responded, not a flat searchDeadline from the start of the search.
	// Before any result exists there's no fast provider to protect, so we
	// wait unboundedly for the first one — each provider's own HTTP client
	// timeout is still the real worst-case bound, since every goroutine
	// below sends to ch when its call returns, success or error.
	var deadlineC <-chan time.Time

	var out []*BookResult
	remaining := len(providers)
	for remaining > 0 {
		select {
		case res := <-ch:
			out = append(out, res.items...)
			remaining--
			if deadlineC == nil {
				deadline := time.NewTimer(searchDeadline)
				defer deadline.Stop()
				deadlineC = deadline.C
			}
		case <-deadlineC:
			slog.InfoContext(ctx, "book search deadline reached, returning partial results",
				"query", query, "waiting_on", remaining, "results_so_far", len(out))
			slog.InfoContext(ctx, "book search done", "query", query, "total", len(out))
			return out
		case <-ctx.Done():
			return out
		}
	}

	slog.InfoContext(ctx, "book search done", "query", query, "total", len(out))
	return out
}

// ContributorProviders returns all enabled providers that implement ContributorProvider.
func (r *Registry) ContributorProviders() []ContributorProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ContributorProvider
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if cp, ok := p.(ContributorProvider); ok {
			out = append(out, cp)
		}
	}
	return out
}

// SeriesVolumesProvider returns the first enabled provider with the given source name that implements SeriesVolumesProvider.
func (r *Registry) SeriesVolumesProvider(source string) SeriesVolumesProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		if p.Info().Name != source {
			continue
		}
		if svp, ok := p.(SeriesVolumesProvider); ok {
			return svp
		}
	}
	return nil
}

// SearchSeries queries all enabled SeriesSearchProviders concurrently.
func (r *Registry) SearchSeries(ctx context.Context, query string) []SeriesResult {
	providers := r.SeriesSearchProviders()
	if len(providers) == 0 {
		return nil
	}

	type result struct {
		items []SeriesResult
	}

	ch := make(chan result, len(providers))
	for _, p := range providers {
		go func(sp SeriesSearchProvider) {
			items, err := sp.SearchSeries(ctx, query)
			if err != nil {
				slog.WarnContext(ctx, "series search provider error",
					"provider", sp.Info().Name, "error", err)
				ch <- result{}
				return
			}
			ch <- result{items: items}
		}(p)
	}

	// The third fan-out, and it had the same unbounded wait LookupISBN did:
	// one provider that hangs holds up a search every other provider has
	// already answered. searchDeadline rather than isbnDeadline because this
	// is a search, and the wider net is worth the wait.
	var deadlineC <-chan time.Time

	var out []SeriesResult
	remaining := len(providers)
	for remaining > 0 {
		select {
		case res := <-ch:
			out = append(out, res.items...)
			remaining--
			if deadlineC == nil {
				deadline := time.NewTimer(searchDeadline)
				defer deadline.Stop()
				deadlineC = deadline.C
			}
		case <-deadlineC:
			slog.InfoContext(ctx, "series search deadline reached, returning partial results",
				"query", query, "waiting_on", remaining, "results_so_far", len(out))
			return out
		case <-ctx.Done():
			return out
		}
	}
	return out
}
