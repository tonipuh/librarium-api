// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package books

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // registers the decoder for the cover probe
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fireball1725/librarium-api/internal/providers"
	"github.com/fireball1725/librarium-api/internal/version"
)

// FinnaProvider looks up books via Finna (finna.fi), the shared discovery
// interface for Finnish libraries, archives, and museums run by the National
// Library of Finland. Free, no API key, no registration.
//
// Finna exists because the other book providers barely cover Finnish-language
// publishing: Open Library / Google Books / Hardcover routinely miss even
// mainstream WSOY/Otava titles, and ISBNdb's Finnish coverage is thin. Finna
// aggregates the actual holdings of Finnish libraries, so for a book bought in
// Finland it is usually the only source that has it.
//
// The field mapping (author extraction from the nested authors object, record
// scoring, cover-image selection) is ported from the finna-isbn microservice
// (github.com/tonipuh/finna-isbn), where it was worked out and tested against
// live Finna responses.
type FinnaProvider struct {
	base
	client    *http.Client
	userAgent string
	// searchURL is the VuFind search endpoint. Not admin-configurable — there
	// is a single canonical public Finna, so unlike the ISFDB mirror there is
	// no user-supplied URL to validate — but a field rather than a const so
	// tests can point it at an httptest server.
	searchURL string
	// coverShowURL is Finna's ISBN cover endpoint, a field for the same reason.
	coverShowURL string

	// minInterval spaces requests (see throttle). A field, not a const, so
	// tests can shrink it instead of sleeping real seconds.
	minInterval time.Duration
	// limMu guards limNext, the earliest instant the next request may start.
	limMu   sync.Mutex
	limNext time.Time
}

// finnaSearchBase is the public Finna search endpoint.
const finnaSearchBase = "https://api.finna.fi/v1/search"

// finnaWebBase prefixes the record's relative cover image path.
const finnaWebBase = "https://www.finna.fi"

// finnaCoverShowBase is Finna's cover service. Asked by ISBN it finds a cover
// for far more books than carry one in images[]: on a sample of Finnish books
// with no images[] entry it had a real cover for 21 of 25.
const finnaCoverShowBase = finnaWebBase + "/Cover/Show"

// finnaCoverMinSide is the smallest width and height a Cover/Show answer may
// have to count as a cover. The no-cover placeholder is a 1x1 GIF; the
// smallest real covers are thumbnails a couple of hundred pixels wide.
const finnaCoverMinSide = 50

// finnaCoverProbeLimit caps how much of a Cover/Show answer is read to find its
// format and size; the image header sits in the first few KB.
const finnaCoverProbeLimit = 256 << 10

// Finna's API terms ask for no more than roughly one request per second. A bulk
// enrichment fans out a lookup per book, so without pacing Finna answers 429
// and the lookups come back empty — which, under a force re-enrich, wiped book
// records. finnaDefaultInterval enforces the cap across all callers sharing the
// provider; finnaMaxRetries bounds the 429 backoff.
const (
	finnaDefaultInterval = time.Second
	finnaMaxRetries      = 4
)

func NewFinnaProvider() *FinnaProvider {
	return &FinnaProvider{
		base:         base{enabled: true},
		client:       &http.Client{Timeout: 10 * time.Second},
		searchURL:    finnaSearchBase,
		coverShowURL: finnaCoverShowBase,
		minInterval:  finnaDefaultInterval,
		// Finna rejects requests without a descriptive User-Agent (HTTP 403),
		// so this is not optional politeness — the lookup fails without it.
		userAgent: fmt.Sprintf("librarium-finna/%s (+https://github.com/fireball1725/librarium-api)", finnaVersion()),
	}
}

func finnaVersion() string {
	if version.Version == "" {
		return version.LocalVersion
	}
	return version.Version
}

func (p *FinnaProvider) Info() providers.ProviderInfo {
	return providers.ProviderInfo{
		Name:         "finna",
		DisplayName:  "Finna",
		Description:  "Finnish libraries' shared catalogue (finna.fi), run by the National Library of Finland. Best source for Finnish-language books, which other providers cover poorly. Free, no API key.",
		RequiresKey:  false,
		Capabilities: []string{providers.CapBookISBN},
		HelpText:     "No configuration needed. Finna aggregates Finnish library holdings and is the strongest source for books published in Finland.",
		HelpURL:      "https://www.finna.fi",
		// The default probe ISBN is a UK edition Finna doesn't carry; test
		// against a Finnish book (Alderton, *Kaikki mitä tiedän rakkaudesta*).
		TestISBN: "9789510450741",
	}
}

func (p *FinnaProvider) Configure(cfg map[string]string) {
	if v, ok := cfg["enabled"]; ok {
		p.enabled = v != "false"
	} else {
		p.enabled = true
	}
}

func (p *FinnaProvider) LookupByISBN(ctx context.Context, isbn string) (*providers.BookResult, error) {
	// Search by the 13-digit form when we can derive it, so a scanned ISBN-10
	// still matches records catalogued under the 13-digit ISBN. If the input
	// isn't a recognisable ISBN we fall back to searching it verbatim.
	norm := normalizeISBN(isbn)
	isbn13, isbn10 := isbnForms(norm)
	lookfor := isbn13
	if lookfor == "" {
		lookfor = norm
	}

	params := url.Values{}
	params.Set("lookfor", lookfor)
	params.Set("type", "ISN") // VuFind's ISBN/ISSN index.
	params.Set("limit", "5")  // A single ISBN can hit several editions; we score and pick one.
	params.Set("lng", "fi")
	// Bracketed repeated params: net/url encodes the key, Finna wants field[].
	for _, f := range []string{
		"id", "title", "shortTitle", "subTitle", "authors", "year",
		"publishers", "languages", "formats", "images", "cleanIsbn", "isbns",
		"subjects", "genres", "summary", "physicalDescriptions",
	} {
		params.Add("field[]", f)
	}

	body, err := p.search(ctx, p.searchURL+"?"+params.Encode())
	if err != nil {
		return nil, err
	}
	if len(body.Records) == 0 {
		return nil, nil // not found is not an error, per the other providers
	}

	rec := selectBestFinnaRecord(body.Records, isbn13, isbn10)
	result := finnaRecordToBookResult(rec, isbn13, isbn10)
	if result.CoverURL == "" && isbn13 != "" {
		result.CoverURL = p.coverByISBN(ctx, isbn13)
	}
	return result, nil
}

// coverByISBN asks Finna's cover service for the ISBN and returns the URL only
// when the answer is a real cover. Cover/Show answers 200 with a 1x1
// transparent GIF when it has no cover, and Librarium downloads and stores
// whatever cover URL it is given, so the URL is never handed on unchecked: the
// answer must decode as a JPEG or PNG at least finnaCoverMinSide on each side.
// Any failure returns "" and leaves the cover for another provider to fill;
// it never fails the lookup, whose metadata is already in hand.
func (p *FinnaProvider) coverByISBN(ctx context.Context, isbn13 string) string {
	if err := p.throttle(ctx); err != nil {
		return ""
	}
	params := url.Values{}
	params.Set("isbn", isbn13)
	params.Set("size", "large")
	coverURL := p.coverShowURL + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", p.userAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	cfg, format, err := image.DecodeConfig(io.LimitReader(resp.Body, finnaCoverProbeLimit))
	if err != nil {
		return ""
	}
	if format != "jpeg" && format != "png" {
		return ""
	}
	if cfg.Width < finnaCoverMinSide || cfg.Height < finnaCoverMinSide {
		return ""
	}
	return coverURL
}

// search performs the rate-limited, 429-aware GET. Every request first waits
// for its throttle slot; a 429 is retried with the Retry-After delay Finna
// sends, or exponential backoff, up to finnaMaxRetries. A non-nil body is
// always a decoded 200 response.
func (p *FinnaProvider) search(ctx context.Context, rawURL string) (*finnaSearchResponse, error) {
	for attempt := 0; ; attempt++ {
		if err := p.throttle(ctx); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", p.userAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := finnaRetryAfter(resp, attempt)
			resp.Body.Close()
			if attempt >= finnaMaxRetries {
				return nil, fmt.Errorf("finna search: rate-limited (429) after %d retries", finnaMaxRetries)
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("finna search: status %d", resp.StatusCode)
		}

		var body finnaSearchResponse
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		return &body, nil
	}
}

// throttle blocks until this provider is allowed to make its next request,
// spacing all concurrent callers minInterval apart. Reserves the slot before
// sleeping so parallel goroutines queue rather than all firing at once.
func (p *FinnaProvider) throttle(ctx context.Context) error {
	if p.minInterval <= 0 {
		return nil
	}
	p.limMu.Lock()
	start := p.limNext
	now := time.Now()
	if start.Before(now) {
		start = now
	}
	p.limNext = start.Add(p.minInterval)
	p.limMu.Unlock()

	return sleepCtx(ctx, time.Until(start))
}

// finnaRetryAfter is how long to wait before retrying a 429: Finna's
// Retry-After header when present, otherwise capped exponential backoff.
func finnaRetryAfter(resp *http.Response, attempt int) time.Duration {
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	d := time.Second << attempt // 1s, 2s, 4s, 8s, …
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// sleepCtx waits for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// selectBestFinnaRecord scores the candidate records and returns the best one.
// A single ISBN search can return the printed book, an e-book, and an
// audiobook as separate records; we prefer the record whose ISBN actually
// matches and, among those, the printed book. Stable: first wins on ties.
func selectBestFinnaRecord(records []finnaRecord, isbn13, isbn10 string) finnaRecord {
	wanted := map[string]bool{}
	if isbn13 != "" {
		wanted[isbn13] = true
	}
	if isbn10 != "" {
		wanted[isbn10] = true
	}

	score := func(r finnaRecord) int {
		s := 0
		candidates := []string{}
		if r.CleanISBN != "" {
			candidates = append(candidates, normalizeISBN(r.CleanISBN))
		}
		for _, raw := range r.ISBNs {
			candidates = append(candidates, normalizeISBN(raw))
		}
		for _, c := range candidates {
			if wanted[c] {
				s += 4
				break
			}
		}
		// formats look like "1/Book/Book/" (printed) or "0/Book/" (family).
		printed, anyBook := false, false
		for _, f := range r.Formats {
			v := strings.ToLower(f.Value)
			if strings.Contains(v, "book/book") {
				printed = true
			} else if strings.Contains(v, "book") {
				anyBook = true
			}
		}
		if printed {
			s += 2
		} else if anyBook {
			s += 1
		}
		return s
	}

	best, bestScore := 0, score(records[0])
	for i := 1; i < len(records); i++ {
		if s := score(records[i]); s > bestScore {
			best, bestScore = i, s
		}
	}
	return records[best]
}

func finnaRecordToBookResult(r finnaRecord, isbn13, isbn10 string) *providers.BookResult {
	result := &providers.BookResult{
		Provider:        "finna",
		ProviderDisplay: "Finna",
		Title:           firstNonEmpty(r.Title, r.ShortTitle),
		Subtitle:        r.SubTitle,
		Authors:         extractFinnaAuthors(r.Authors),
		Publisher:       firstFinnaString(r.Publishers),
		ISBN13:          isbn13,
		ISBN10:          isbn10,
		Language:        finnaLanguage(r.Languages),
	}
	// Finna records carry only a year, not a full date; normalise to the
	// YYYY-MM-DD the rest of the pipeline expects (matching Open Library's
	// year-only handling).
	if y := parseFinnaYear(r.Year); y != "" {
		result.PublishDate = y + "-01-01"
	}
	// genres before subjects: the genre ("muistelmat") is the most useful
	// signal for the client's media-type detection, and subjects are the long
	// tail of topical headings. Both feed Categories.
	result.Categories = extractFinnaCategories(r.Genres, r.Subjects)
	if len(r.Summary) > 0 {
		result.Description = strings.TrimSpace(r.Summary[0])
	}
	result.PageCount = parseFinnaPageCount(r.PhysicalDescriptions)
	result.CoverURL = finnaCoverURL(r)
	return result
}

// extractFinnaCategories flattens Finna's genres and subjects into a single
// deduped Categories list. subjects arrives as a list of single-element lists
// ([["rakkaus"], ["ystävyys"], ...]) and mixes Finnish and Swedish translations
// of the same heading; we keep them all (order preserved, duplicates dropped)
// rather than trying to guess which language the caller wants.
func extractFinnaCategories(genres []string, subjects [][]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(genres)+len(subjects))
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, g := range genres {
		add(g)
	}
	for _, group := range subjects {
		for _, s := range group {
			add(s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseFinnaPageCount pulls a page count out of Finna's free-text physical
// description ("320 sivua", "320 s.", "1 verkkoaineisto (320 sivua)"). Returns
// nil when no leading run of digits is present.
func parseFinnaPageCount(descs []string) *int {
	for _, d := range descs {
		if m := reFinnaPages.FindStringSubmatch(d); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return &n
			}
		}
	}
	return nil
}

// First run of digits that is immediately followed by a Finnish/Swedish page
// word (sivu/s./sidor/p.), so "320 sivua" matches but "1 verkkoaineisto" and a
// bare year don't.
var reFinnaPages = regexp.MustCompile(`(\d+)\s*(?:sivu|s\.|sidor|sid\.|p\.|pages)`)

// extractFinnaAuthors pulls the author names out of Finna's nested authors
// object, whose groups are JSON objects keyed by the name.
//
// Only the primary group is used. Secondary keys are the full library heading
// form with birth year and role baked into the string
// ("Viitanen, Viia 1973- kääntäjä"), which is neither a clean author name nor,
// for a translator/editor, an author of the work at all. The corporate group
// is the fallback for the rare work catalogued solely under an organisation.
//
// Personal names are flipped from Finna's library "Surname, Given" form to the
// "Given Surname" form the other providers emit. This matters beyond looks:
// MergeBookResults joins authors with ", " and the clients split on it, so a
// name that itself contains a comma ("Alderton, Dolly") round-trips into two
// broken half-names. Corporate names are left as-is (an organisation isn't
// "Surname, Given").
func extractFinnaAuthors(a finnaAuthors) []string {
	names := a.Primary.keys()
	flip := true
	if len(names) == 0 {
		names = a.Corporate.keys()
		flip = false
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if flip {
			n = naturalName(n)
		}
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// naturalName turns "Surname, Given" into "Given Surname". Names without a
// single splitting comma (already natural, or mononyms) are returned unchanged.
func naturalName(s string) string {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(s)
	}
	last := strings.TrimSpace(parts[0])
	first := strings.TrimSpace(parts[1])
	if last == "" || first == "" {
		return strings.TrimSpace(s)
	}
	return first + " " + last
}

// finnaCoverURL returns the record's own cover image, made absolute against
// finna.fi. Returns "" when the record has no image; LookupByISBN then tries
// Cover/Show by ISBN through coverByISBN, which checks the answer is a real
// cover rather than the blank placeholder that endpoint sends when it has none.
func finnaCoverURL(r finnaRecord) string {
	if len(r.Images) == 0 {
		return ""
	}
	img := r.Images[0]
	switch {
	case strings.HasPrefix(img, "http://"), strings.HasPrefix(img, "https://"):
		return img
	case strings.HasPrefix(img, "/"):
		return finnaWebBase + img
	default:
		return finnaWebBase + "/" + img
	}
}

// finnaLanguage maps Finna's ISO 639-2/B three-letter codes to the two-letter
// ISO 639-1 codes the clients use, matching Open Library's language handling.
// Unknown codes pass through unchanged.
func finnaLanguage(langs []string) string {
	if len(langs) == 0 {
		return ""
	}
	iso := map[string]string{
		"fin": "fi", "swe": "sv", "eng": "en", "ger": "de", "fre": "fr",
		"spa": "es", "ita": "it", "por": "pt", "rus": "ru", "jpn": "ja",
		"chi": "zh", "kor": "ko", "dan": "da", "nor": "no", "est": "et",
	}
	if v, ok := iso[langs[0]]; ok {
		return v
	}
	return langs[0]
}

// parseFinnaYear returns the leading four-digit year, or "" if there isn't one.
func parseFinnaYear(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return ""
	}
	y := s[:4]
	if _, err := strconv.Atoi(y); err != nil {
		return ""
	}
	return y
}

// ─── ISBN helpers ─────────────────────────────────────────────────────────────

// normalizeISBN strips hyphens and whitespace and upper-cases the ISBN-10
// check character X.
func normalizeISBN(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'x' || r == 'X':
			b.WriteRune('X')
		}
	}
	return b.String()
}

// isbnForms returns the 13- and 10-digit forms for a normalised ISBN, deriving
// whichever is missing. Either return value is "" when it can't be produced
// (an invalid ISBN, or a 979-prefixed ISBN-13 which has no ISBN-10 form).
func isbnForms(norm string) (isbn13, isbn10 string) {
	switch len(norm) {
	case 13:
		if isValidISBN13(norm) {
			return norm, isbn13To10(norm)
		}
	case 10:
		if isValidISBN10(norm) {
			return isbn10To13(norm), norm
		}
	}
	return "", ""
}

func isbn10CheckDigit(body9 string) byte {
	sum := 0
	for i := 0; i < 9; i++ {
		sum += (10 - i) * int(body9[i]-'0')
	}
	r := (11 - (sum % 11)) % 11
	if r == 10 {
		return 'X'
	}
	return byte('0' + r)
}

func isbn13CheckDigit(body12 string) byte {
	sum := 0
	for i := 0; i < 12; i++ {
		w := 1
		if i%2 == 1 {
			w = 3
		}
		sum += w * int(body12[i]-'0')
	}
	return byte('0' + (10-(sum%10))%10)
}

func isValidISBN10(v string) bool {
	if len(v) != 10 {
		return false
	}
	for i := 0; i < 9; i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	last := v[9]
	if (last < '0' || last > '9') && last != 'X' {
		return false
	}
	return isbn10CheckDigit(v[:9]) == last
}

func isValidISBN13(v string) bool {
	if len(v) != 13 {
		return false
	}
	for i := 0; i < 13; i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	if !strings.HasPrefix(v, "978") && !strings.HasPrefix(v, "979") {
		return false
	}
	return isbn13CheckDigit(v[:12]) == v[12]
}

func isbn10To13(isbn10 string) string {
	body := "978" + isbn10[:9]
	return body + string(isbn13CheckDigit(body))
}

// isbn13To10 converts a 978-prefixed ISBN-13 to ISBN-10. The 979 prefix has no
// ISBN-10 equivalent, so it returns "".
func isbn13To10(isbn13 string) string {
	if !strings.HasPrefix(isbn13, "978") {
		return ""
	}
	body := isbn13[3:12]
	return body + string(isbn10CheckDigit(body))
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstFinnaString(vals []string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ─── Finna API types ──────────────────────────────────────────────────────────

type finnaSearchResponse struct {
	ResultCount int           `json:"resultCount"`
	Records     []finnaRecord `json:"records"`
	Status      string        `json:"status"`
}

type finnaRecord struct {
	ID         string        `json:"id"`
	Title      string        `json:"title"`
	ShortTitle string        `json:"shortTitle"`
	SubTitle   string        `json:"subTitle"`
	Authors    finnaAuthors  `json:"authors"`
	Year       string        `json:"year"`
	Publishers []string      `json:"publishers"`
	Languages  []string      `json:"languages"`
	Formats    []finnaFormat `json:"formats"`
	Images     []string      `json:"images"`
	CleanISBN  string        `json:"cleanIsbn"`
	ISBNs      []string      `json:"isbns"`
	// subjects is a list of single-element lists in Finna's JSON.
	Subjects             [][]string `json:"subjects"`
	Genres               []string   `json:"genres"`
	Summary              []string   `json:"summary"`
	PhysicalDescriptions []string   `json:"physicalDescriptions"`
}

type finnaFormat struct {
	Value      string `json:"value"`
	Translated string `json:"translated"`
}

// finnaAuthors is Finna's authors object. Each group is a map keyed by the
// author name (the value holds roles, which we don't use for the book record).
type finnaAuthors struct {
	Primary   finnaAuthorGroup `json:"primary"`
	Secondary finnaAuthorGroup `json:"secondary"`
	Corporate finnaAuthorGroup `json:"corporate"`
}

// finnaAuthorGroup decodes to either a name-keyed object or, when Finna returns
// an empty group, a JSON array ([]). A custom unmarshaller tolerates both so a
// record with no corporate authors doesn't fail the whole decode.
type finnaAuthorGroup struct {
	names []string
}

func (g *finnaAuthorGroup) keys() []string { return g.names }

func (g *finnaAuthorGroup) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" || strings.HasPrefix(trimmed, "[") {
		// Empty group: Finna serialises it as [] rather than {}.
		g.names = nil
		return nil
	}
	// json.Unmarshal into a map loses key order, so decode the object with a
	// token stream to keep the author order Finna returned.
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if _, err := dec.Token(); err != nil { // opening '{'
		return err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return err
		}
		g.names = append(g.names, key.(string))
		// Skip the value (role object) — we only need the key.
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}
