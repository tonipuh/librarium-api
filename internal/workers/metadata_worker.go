// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 FireBall1725 (Adaléa)

package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fireball1725/librarium-api/internal/models"
	"github.com/fireball1725/librarium-api/internal/providers"
	"github.com/fireball1725/librarium-api/internal/repository"
	"github.com/fireball1725/librarium-api/internal/service"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// ErrNoUpdate is returned by ProcessBook when processing succeeded but nothing
// was changed — e.g. no ISBN to look up, or no cover available from providers.
// It is not a failure and should not trigger River retries.
var ErrNoUpdate = errors.New("no update")

// MetadataWorker enriches a single book with metadata from configured providers.
// It is enqueued by the import worker (when EnrichMetadata=true) and can also
// be enqueued manually from the API for individual books.
type MetadataWorker struct {
	river.WorkerDefaults[models.MetadataEnrichmentJobArgs]

	books        *repository.BookRepo
	contributors *repository.ContributorRepo
	editions     *repository.EditionRepo
	genres       *repository.GenreRepo
	providerSvc  *service.ProviderService
	bookSvc      *service.BookService
}

func NewMetadataWorker(
	books *repository.BookRepo,
	contributors *repository.ContributorRepo,
	editions *repository.EditionRepo,
	genres *repository.GenreRepo,
	providerSvc *service.ProviderService,
	bookSvc *service.BookService,
) *MetadataWorker {
	return &MetadataWorker{
		books:        books,
		contributors: contributors,
		editions:     editions,
		genres:       genres,
		providerSvc:  providerSvc,
		bookSvc:      bookSvc,
	}
}

func (w *MetadataWorker) Work(ctx context.Context, job *river.Job[models.MetadataEnrichmentJobArgs]) error {
	args := job.Args
	slog.Info("metadata enrichment started", "book_id", args.BookID, "force", args.Force, "cover_only", args.CoverOnly)
	err := w.ProcessBook(ctx, args.BookID, args.CallerID, args.Force, args.CoverOnly)
	if err != nil && !errors.Is(err, ErrNoUpdate) {
		return err // only real errors cause River retries
	}
	slog.Info("metadata enrichment done", "book_id", args.BookID)
	return nil
}

// ProcessBook enriches or refreshes the cover for a single book.
// Returns ErrNoUpdate when processing succeeded but nothing changed (no ISBN,
// no covers from providers, already has cover). Returns a real error only for
// transient failures worth retrying.
// ProcessBook enriches or refreshes the cover for a single book. libraryID is
// no longer a parameter (books belong to zero or more libraries under M2M,
// and the enrichment itself writes to the global book row regardless).
func (w *MetadataWorker) ProcessBook(ctx context.Context, bookID, callerID uuid.UUID, force, coverOnly bool) error {
	// Find an ISBN from the book's editions.
	editions, err := w.editions.ListByBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("loading editions: %w", err)
	}

	var isbn string
	for _, e := range editions {
		if e.ISBN13 != "" {
			isbn = e.ISBN13
			break
		}
		if e.ISBN10 != "" {
			isbn = e.ISBN10
		}
	}

	if isbn == "" {
		return ErrNoUpdate // no ISBN — nothing to look up
	}

	merged, err := w.providerSvc.LookupISBNMerged(ctx, isbn)
	if err != nil {
		return fmt.Errorf("ISBN lookup: %w", err)
	}

	// When cover_only=true, skip all text-field updates and only refresh the cover.
	if !coverOnly {
		if err := w.applyMerged(ctx, bookID, callerID, merged, force); err != nil {
			return fmt.Errorf("applying merged result: %w", err)
		}
	}

	// Fetch cover if the book has none, or when force is set. cover_only
	// no longer forces a refetch — a book that already has a cover is
	// left alone in any post-import / batch enrichment, matching the
	// "fill missing data only" contract the import UI advertises.
	// Only an explicit Force=true (e.g. a manual re-fetch) overrides.
	if len(merged.Covers) == 0 {
		if coverOnly {
			return ErrNoUpdate // nothing to do for a cover-only job
		}
		return nil // metadata was applied; absence of cover is not a failure
	}

	hasCover := false
	if _, _, err := w.bookSvc.GetBookCoverPath(ctx, bookID); err == nil {
		hasCover = true
	}
	if hasCover && !force {
		if coverOnly {
			return ErrNoUpdate // already has a cover; cover-only batch is a no-op
		}
		return nil
	}
	if !hasCover || force {
		// Try covers in priority order until one downloads successfully —
		// the top-priority URL (e.g. Google Books thumbnail) can 403/404, and
		// falling through to the next provider is better than giving up.
		fetched := false
		for _, cover := range merged.Covers {
			if err := w.bookSvc.FetchCoverFromURL(ctx, bookID, callerID, cover.CoverURL); err != nil {
				slog.Warn("cover fetch failed during enrichment",
					"book_id", bookID, "source", cover.Source, "url", cover.CoverURL, "error", err)
				continue
			}
			fetched = true
			break
		}
		if !fetched && coverOnly {
			return ErrNoUpdate // every provider's URL failed — treat as no-change
		}
	} else if coverOnly {
		// Already has a cover and not forcing — nothing changed.
		return ErrNoUpdate
	}

	return nil
}

// fillField decides the new value for one metadata field. force overwrites an
// existing value, but only with a real one — a provider that returned nothing
// (rate-limited, or a book it doesn't hold) must never blank out metadata we
// already have. A bulk force re-enrich once wiped title and ISBN on every book
// whose lookup came back empty; this guard is what stops an empty merged result
// from erasing a good record.
func fillField(current, merged string, force bool) string {
	if merged != "" && (force || current == "") {
		return merged
	}
	return current
}

// applyMerged updates the book and its primary edition with merged provider data.
// When force=false only empty fields are filled; when force=true all fields are overwritten.
func (w *MetadataWorker) applyMerged(
	ctx context.Context,
	bookID, callerID uuid.UUID,
	merged *providers.MergedBookResult,
	force bool,
) error {
	_ = callerID // reserved for audit trail; applyMerged itself doesn't use it
	book, err := w.bookSvc.GetBook(ctx, bookID, uuid.Nil, uuid.Nil)
	if err != nil {
		return err
	}

	// ── Book-level fields ─────────────────────────────────────────────────────

	fill := func(current, merged string) string {
		return fillField(current, merged, force)
	}

	newTitle := fill(book.Title, fieldVal(merged.Title))
	newSubtitle := fill(book.Subtitle, fieldVal(merged.Subtitle))
	newDescription := fill(book.Description, fieldVal(merged.Description))

	// Contributors: only apply when force=true or book has none.
	contribs := make([]repository.ContributorInput, len(book.Contributors))
	for i, c := range book.Contributors {
		contribs[i] = repository.ContributorInput{
			ContributorID: c.ContributorID,
			Role:          c.Role,
			DisplayOrder:  c.DisplayOrder,
		}
	}
	if (force || len(contribs) == 0) && merged.Authors != nil && merged.Authors.Value != "" {
		contribs = nil
		for i, name := range splitAuthors(merged.Authors.Value) {
			c, err := w.findOrCreateContributor(ctx, name)
			if err != nil {
				slog.Warn("contributor resolution failed", "name", name, "error", err)
				continue
			}
			contribs = append(contribs, repository.ContributorInput{
				ContributorID: c.ID,
				Role:          "author",
				DisplayOrder:  i,
			})
		}
	}

	// Genres: supplement existing (union) or replace on force.
	allGenres, err := w.genres.List(ctx)
	if err != nil {
		return fmt.Errorf("loading genres: %w", err)
	}
	existingGenreIDs := make([]uuid.UUID, len(book.Genres))
	for i, g := range book.Genres {
		existingGenreIDs[i] = g.ID
	}
	newGenreIDs := existingGenreIDs
	if len(merged.Categories) > 0 {
		fromProvider := normalizeCategories(merged.Categories, allGenres)
		if force {
			newGenreIDs = fromProvider
		} else {
			seen := make(map[uuid.UUID]bool, len(existingGenreIDs))
			for _, id := range existingGenreIDs {
				seen[id] = true
			}
			for _, id := range fromProvider {
				if !seen[id] {
					seen[id] = true
					newGenreIDs = append(newGenreIDs, id)
				}
			}
		}
	}

	// Preserve existing tag IDs (enrichment never changes tags).
	tagIDs := make([]uuid.UUID, len(book.Tags))
	for i, t := range book.Tags {
		tagIDs[i] = t.ID
	}

	if _, err := w.bookSvc.UpdateBook(ctx, bookID, service.BookRequest{
		Title:        newTitle,
		Subtitle:     newSubtitle,
		Description:  newDescription,
		MediaTypeID:  book.MediaTypeID,
		Contributors: contribs,
		TagIDs:       tagIDs,
		GenreIDs:     newGenreIDs,
	}); err != nil {
		return fmt.Errorf("updating book: %w", err)
	}

	// ── Edition-level fields (primary edition) ────────────────────────────────

	editions, err := w.editions.ListByBook(ctx, bookID)
	if err != nil {
		return fmt.Errorf("loading editions: %w", err)
	}
	var primary *models.BookEdition
	for _, e := range editions {
		if e.IsPrimary {
			primary = e
			break
		}
	}
	if primary == nil && len(editions) > 0 {
		primary = editions[0]
	}
	if primary == nil {
		return nil // no edition to update
	}

	newPublisher := fill(primary.Publisher, fieldVal(merged.Publisher))
	newLanguage := fill(primary.Language, fieldVal(merged.Language))
	newISBN10 := fill(primary.ISBN10, fieldVal(merged.ISBN10))
	newISBN13 := fill(primary.ISBN13, fieldVal(merged.ISBN13))

	var newPublishDate *time.Time
	if primary.PublishDate != nil {
		newPublishDate = primary.PublishDate
	}
	if (force || primary.PublishDate == nil) && merged.PublishDate != nil && merged.PublishDate.Value != "" {
		for _, layout := range []string{"2006-01-02", "2006-01", "2006", "January 2, 2006", "Jan 2, 2006"} {
			if t, err := time.Parse(layout, merged.PublishDate.Value); err == nil {
				newPublishDate = &t
				break
			}
		}
	}

	newPageCount := primary.PageCount
	if (force || primary.PageCount == nil) && merged.PageCount != nil && merged.PageCount.Value != "" {
		var n int
		if _, err := fmt.Sscanf(merged.PageCount.Value, "%d", &n); err == nil && n > 0 {
			newPageCount = &n
		}
	}

	if _, err := w.bookSvc.UpdateEdition(ctx, primary.ID, service.EditionRequest{
		Format:          primary.Format,
		Language:        newLanguage,
		EditionName:     primary.EditionName,
		Narrator:        primary.Narrator,
		Publisher:       newPublisher,
		PublishDate:     newPublishDate,
		ISBN10:          newISBN10,
		ISBN13:          newISBN13,
		Description:     primary.Description,
		DurationSeconds: primary.DurationSeconds,
		PageCount:       newPageCount,
		IsPrimary:       primary.IsPrimary,
	}); err != nil {
		return fmt.Errorf("updating edition: %w", err)
	}

	return nil
}

func (w *MetadataWorker) findOrCreateContributor(ctx context.Context, name string) (*models.Contributor, error) {
	results, err := w.contributors.Search(ctx, name, 5)
	if err != nil {
		return nil, err
	}
	for _, c := range results {
		if strings.EqualFold(c.Name, name) {
			return c, nil
		}
	}
	return w.contributors.Create(ctx, uuid.New(), name, service.DeriveSortName(name), false)
}

// fieldVal safely extracts the value from a nullable FieldResult.
func fieldVal(f *providers.FieldResult) string {
	if f == nil {
		return ""
	}
	return f.Value
}
