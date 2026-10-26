// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// finnaResponse is a trimmed but faithful sample of a Finna /v1/search reply,
// captured from api.finna.fi for a real Finnish ISBN.
const finnaResponse = `{
  "resultCount": 2,
  "records": [
    {
      "id": "anders.audiobook-id",
      "title": "Kaikki mitä tiedän rakkaudesta",
      "authors": {"primary": {"Alderton, Dolly": {"role": ["kirjoittaja"]}}, "secondary": [], "corporate": []},
      "year": "2020",
      "publishers": ["WSOY"],
      "languages": ["fin"],
      "formats": [{"value": "1/Book/AudioBook/", "translated": "Äänikirja"}],
      "images": [],
      "cleanIsbn": "9789510450741",
      "isbns": ["9789510450741"]
    },
    {
      "id": "anders.7a1c2448-42fe-4176-a9f3-b415a9123269",
      "title": "Kaikki mitä tiedän rakkaudesta",
      "authors": {
        "primary": {"Alderton, Dolly": {"role": ["kirjoittaja"]}, "Viitanen, Viia": {"role": ["-"]}},
        "secondary": {"Viitanen, Viia 1973- kääntäjä": {"role": ["kääntäjä"]}},
        "corporate": []
      },
      "year": "2020",
      "publishers": ["WSOY"],
      "languages": ["fin"],
      "formats": [{"value": "0/Book/", "translated": "Kirja"}, {"value": "1/Book/Book/", "translated": "Kirja"}],
      "images": ["/Cover/Show?id=anders.7a1c2448"],
      "cleanIsbn": "9789510450741",
      "isbns": ["9789510450741"],
      "subjects": [["rakkaus"], ["ystävyys"], ["rakkaus"]],
      "genres": ["muistelmat", "memoarer"],
      "summary": ["Underhållande och insiktsfull debut."],
      "physicalDescriptions": ["320 sivua"]
    }
  ],
  "status": "OK"
}`

func newTestFinnaProvider(t *testing.T, handler http.HandlerFunc) *FinnaProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := NewFinnaProvider()
	p.searchURL = srv.URL
	p.coverShowURL = srv.URL + "/Cover/Show"
	p.minInterval = 0 // don't pace test requests
	return p
}

func TestFinnaProvider_LookupByISBN(t *testing.T) {
	var gotUA, gotType, gotLookfor string
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotType = r.URL.Query().Get("type")
		gotLookfor = r.URL.Query().Get("lookfor")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(finnaResponse))
	})

	got, err := p.LookupByISBN(context.Background(), "978-951-0-45074-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want a book result")
	}

	// Finna rejects requests without a User-Agent, so it must always be sent.
	if gotUA == "" {
		t.Error("no User-Agent header sent; Finna returns 403 without one")
	}
	if gotType != "ISN" {
		t.Errorf("type = %q, want ISN", gotType)
	}
	// Hyphenated ISBN-13 should be normalised before it hits the API.
	if gotLookfor != "9789510450741" {
		t.Errorf("lookfor = %q, want normalised 9789510450741", gotLookfor)
	}

	if got.Title != "Kaikki mitä tiedän rakkaudesta" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Provider != "finna" || got.ProviderDisplay != "Finna" {
		t.Errorf("Provider = %q / %q", got.Provider, got.ProviderDisplay)
	}
	// The printed book (1/Book/Book/) must win over the audiobook record, and
	// its primary authors come back flipped to "Given Surname" order.
	if len(got.Authors) != 2 || got.Authors[0] != "Dolly Alderton" || got.Authors[1] != "Viia Viitanen" {
		t.Errorf("Authors = %#v, want [Dolly Alderton, Viia Viitanen]", got.Authors)
	}
	if got.Publisher != "WSOY" {
		t.Errorf("Publisher = %q, want WSOY", got.Publisher)
	}
	if got.PublishDate != "2020-01-01" {
		t.Errorf("PublishDate = %q, want 2020-01-01", got.PublishDate)
	}
	if got.Language != "fi" {
		t.Errorf("Language = %q, want fi (mapped from fin)", got.Language)
	}
	if got.ISBN13 != "9789510450741" {
		t.Errorf("ISBN13 = %q", got.ISBN13)
	}
	if got.ISBN10 != "951045074X" {
		t.Errorf("ISBN10 = %q, want 951045074X (derived)", got.ISBN10)
	}
	// Relative image path made absolute against finna.fi.
	if got.CoverURL != "https://www.finna.fi/Cover/Show?id=anders.7a1c2448" {
		t.Errorf("CoverURL = %q", got.CoverURL)
	}
	// genres first, then subjects, deduped ("rakkaus" appears twice in subjects).
	wantCats := []string{"muistelmat", "memoarer", "rakkaus", "ystävyys"}
	if len(got.Categories) != len(wantCats) {
		t.Fatalf("Categories = %#v, want %#v", got.Categories, wantCats)
	}
	for i, c := range wantCats {
		if got.Categories[i] != c {
			t.Errorf("Categories[%d] = %q, want %q", i, got.Categories[i], c)
		}
	}
	if got.Description != "Underhållande och insiktsfull debut." {
		t.Errorf("Description = %q", got.Description)
	}
	if got.PageCount == nil || *got.PageCount != 320 {
		t.Errorf("PageCount = %v, want 320 (from \"320 sivua\")", got.PageCount)
	}
}

func TestFinnaProvider_RetriesOn429(t *testing.T) {
	var calls int
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0") // don't actually sleep in the test
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(finnaResponse))
	})

	got, err := p.LookupByISBN(context.Background(), "9789510450741")
	if err != nil {
		t.Fatalf("expected the retry to succeed, got: %v", err)
	}
	if got == nil || calls != 2 {
		t.Errorf("calls = %d, want 2 (one 429 then a successful retry)", calls)
	}
}

func TestFinnaProvider_429Exhausted(t *testing.T) {
	var calls int
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})

	got, err := p.LookupByISBN(context.Background(), "9789510450741")
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if got != nil {
		t.Errorf("got %+v, want nil on persistent 429", got)
	}
	if calls != finnaMaxRetries+1 {
		t.Errorf("calls = %d, want %d (initial + %d retries)", calls, finnaMaxRetries+1, finnaMaxRetries)
	}
}

func TestFinnaThrottle_Spaces(t *testing.T) {
	p := NewFinnaProvider()
	p.minInterval = 40 * time.Millisecond
	ctx := context.Background()

	if err := p.throttle(ctx); err != nil { // first slot is immediate
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.throttle(ctx); err != nil { // second must wait ~minInterval
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Errorf("second throttle waited %v, want >= ~40ms", waited)
	}
}

func TestFinnaThrottle_CancelledContext(t *testing.T) {
	p := NewFinnaProvider()
	p.minInterval = time.Hour // force a long wait so cancellation is what returns
	_ = p.throttle(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.throttle(ctx); err == nil {
		t.Error("throttle should return the context error when cancelled while waiting")
	}
}

func TestNaturalName(t *testing.T) {
	cases := map[string]string{
		"Alderton, Dolly":   "Dolly Alderton",
		"Tolkien, J. R. R.": "J. R. R. Tolkien",
		"Dolly Alderton":    "Dolly Alderton", // already natural
		"Madonna":           "Madonna",        // mononym
		"WSOY":              "WSOY",
	}
	for in, want := range cases {
		if got := naturalName(in); got != want {
			t.Errorf("naturalName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFinnaPageCount(t *testing.T) {
	cases := []struct {
		in   []string
		want int // 0 means want nil
	}{
		{[]string{"320 sivua"}, 320},
		{[]string{"320 s."}, 320},
		{[]string{"1 verkkoaineisto (280 sivua)"}, 280},
		{[]string{"320 sidor"}, 320},
		{[]string{"1 verkkoaineisto"}, 0}, // no page word after the digit
		{[]string{""}, 0},
		{nil, 0},
	}
	for _, tc := range cases {
		got := parseFinnaPageCount(tc.in)
		if tc.want == 0 {
			if got != nil {
				t.Errorf("parseFinnaPageCount(%q) = %v, want nil", tc.in, *got)
			}
			continue
		}
		if got == nil || *got != tc.want {
			t.Errorf("parseFinnaPageCount(%q) = %v, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFinnaProvider_LookupByISBN_NotFound(t *testing.T) {
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resultCount":0,"records":[],"status":"OK"}`))
	})

	got, err := p.LookupByISBN(context.Background(), "9780000000002")
	if err != nil {
		t.Fatalf("empty result set should not be an error, got: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for no records", got)
	}
}

func TestFinnaProvider_LookupByISBN_ServerError(t *testing.T) {
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	got, err := p.LookupByISBN(context.Background(), "9789510450741")
	if err == nil {
		t.Fatal("expected an error for a 500 response")
	}
	if got != nil {
		t.Errorf("got %+v, want nil alongside the error", got)
	}
}

// finnaNoImageRecord is a search reply whose record has no images[] entry, so
// the lookup falls back to Cover/Show by ISBN.
const finnaNoImageRecord = `{"resultCount":1,"status":"OK","records":[{
	"title":"X","cleanIsbn":"9789510450741","isbns":["9789510450741"],
	"formats":[{"value":"1/Book/Book/"}],"images":[]}]}`

// finnaPlaceholderGIF is the 1x1 transparent GIF Cover/Show answers with,
// status 200, when it has no cover for the ISBN.
var finnaPlaceholderGIF, _ = base64.StdEncoding.DecodeString(
	"R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newCoverShowProvider answers searches with finnaNoImageRecord and Cover/Show
// with cover, recording the Cover/Show query it was sent.
func newCoverShowProvider(t *testing.T, status int, cover []byte, gotQuery *string) *FinnaProvider {
	t.Helper()
	return newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Cover/Show" {
			if gotQuery != nil {
				*gotQuery = r.URL.RawQuery
			}
			w.WriteHeader(status)
			_, _ = w.Write(cover)
			return
		}
		_, _ = w.Write([]byte(finnaNoImageRecord))
	})
}

func TestFinnaProvider_CoverShowByISBN(t *testing.T) {
	var gotQuery string
	p := newCoverShowProvider(t, http.StatusOK, testJPEG(t, 235, 333), &gotQuery)

	got, err := p.LookupByISBN(context.Background(), "978-951-0-45074-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotQuery != "isbn=9789510450741&size=large" {
		t.Errorf("Cover/Show query = %q, want the ISBN-13 at size=large", gotQuery)
	}
	want := p.coverShowURL + "?isbn=9789510450741&size=large"
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
}

func TestFinnaProvider_NoCoverFromCoverShow(t *testing.T) {
	// Each of these is Cover/Show without a real cover. The URL must not be
	// handed on: Librarium would download and store the blank as the cover.
	cases := map[string]struct {
		status int
		body   []byte
	}{
		"placeholder gif": {http.StatusOK, finnaPlaceholderGIF},
		"tiny jpeg":       {http.StatusOK, testJPEG(t, 10, 10)},
		"not an image":    {http.StatusOK, []byte("<html>error</html>")},
		"not found":       {http.StatusNotFound, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := newCoverShowProvider(t, c.status, c.body, nil)
			got, err := p.LookupByISBN(context.Background(), "9789510450741")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil || got.Title != "X" {
				t.Fatalf("got %+v, want the record's metadata despite no cover", got)
			}
			if got.CoverURL != "" {
				t.Errorf("CoverURL = %q, want empty", got.CoverURL)
			}
		})
	}
}

func TestFinnaProvider_CoverFromRecordImage(t *testing.T) {
	p := newTestFinnaProvider(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"resultCount":1,"status":"OK","records":[{
			"title":"X","cleanIsbn":"9789510450741","isbns":["9789510450741"],
			"formats":[{"value":"1/Book/Book/"}],
			"images":["/Cover/Show?source=Solr&id=eepos.2522540&index=0&size=large"]}]}`))
	})

	got, err := p.LookupByISBN(context.Background(), "9789510450741")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "https://www.finna.fi/Cover/Show?source=Solr&id=eepos.2522540&index=0&size=large"
	if got.CoverURL != want {
		t.Errorf("CoverURL = %q, want %q", got.CoverURL, want)
	}
}

func TestNormalizeISBN(t *testing.T) {
	cases := map[string]string{
		"978-951-0-45074-1": "9789510450741",
		" 951045074x ":      "951045074X",
		"9510450741":        "9510450741",
	}
	for in, want := range cases {
		if got := normalizeISBN(in); got != want {
			t.Errorf("normalizeISBN(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestISBNForms(t *testing.T) {
	cases := []struct {
		in             string
		want13, want10 string
	}{
		{"9789510450741", "9789510450741", "951045074X"},
		{"951045074X", "9789510450741", "951045074X"},
		{"9791234567896", "9791234567896", ""}, // 979 has no ISBN-10 form
		{"1234567890123", "", ""},              // bad checksum/prefix
		{"notanisbn", "", ""},
	}
	for _, tc := range cases {
		got13, got10 := isbnForms(normalizeISBN(tc.in))
		if got13 != tc.want13 || got10 != tc.want10 {
			t.Errorf("isbnForms(%q) = %q/%q, want %q/%q", tc.in, got13, got10, tc.want13, tc.want10)
		}
	}
}
