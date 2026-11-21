package api

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/clappingmonkey/deplexity/internal/models"
)

const apiVersion = "2.18"

type getter interface {
	Get(context.Context, string, any) error
}

type poster interface {
	Post(context.Context, string, any, any) error
}

// ListThreads fetches all user threads via POST /rest/thread/list_ask_threads.
// Stops when a page returns fewer than limit results (end of data) or when all results are
// duplicates (safety net for API recycling behavior).
// If onProgress is non-nil, it is called after each page with the total count so far.
func ListThreads(ctx context.Context, c poster, onProgress func(int)) ([]models.Thread, error) {
	var allThreads []models.Thread
	offset := 0
	limit := 20
	seenUUIDs := make(map[string]bool)

	path := fmt.Sprintf("/rest/thread/list_ask_threads?version=%s&source=default", apiVersion)

	for {
		select {
		case <-ctx.Done():
			return allThreads, ctx.Err()
		default:
		}

		reqBody := ThreadListRequest{
			Limit:         limit,
			Ascending:     false,
			Offset:        offset,
			SearchTerm:    "",
			ExcludeASI:    false,
			IncludeAssets: true,
		}

		var raw ThreadListResponse
		if err := c.Post(ctx, path, reqBody, &raw); err != nil {
			return allThreads, fmt.Errorf("failed to list threads: %w", err)
		}

		if len(raw) == 0 {
			break
		}

		newCount := 0
		for _, t := range raw {
			if seenUUIDs[t.UUID] {
				continue
			}
			seenUUIDs[t.UUID] = true
			newCount++

			thread := models.Thread{
				UUID:  t.UUID,
				Title: t.Title,
				Slug:  t.Slug,
			}
			if t.Collection != nil {
				thread.SpaceUUID = t.Collection.UUID
			}
			if t.LastQueryTime != "" {
				thread.UpdatedAt = parseTime(t.LastQueryTime)
			}
			allThreads = append(allThreads, thread)
		}

		if onProgress != nil {
			onProgress(len(allThreads))
		}

		// If no new threads were found, the API is recycling — stop.
		if newCount == 0 {
			break
		}

		// If we got fewer than limit, we've reached the last page.
		if len(raw) < limit {
			break
		}

		offset += len(raw)
	}

	return allThreads, nil
}

// threadPagePath builds the detail endpoint URL for a single page of a thread.
//
// The endpoint ignores the offset parameter and paginates by cursor: each
// response returns a next_cursor that must be passed back verbatim. offset is
// pinned to 0, mirroring the browser. The cursor is URL-encoded JSON, so it is
// escaped here because the HTTP client concatenates the path onto the base URL
// without any encoding of its own.
func threadPagePath(uuid, cursor string) string {
	path := fmt.Sprintf("/rest/thread/%s?with_schematized_response=true&version=%s&source=default&limit=100&offset=0&from_first=true&supported_block_use_cases=answer_modes&supported_block_use_cases=preserve_latex", uuid, apiVersion)
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	return path
}

// EntryHook is called by GetThread for every newly fetched entry, right after
// the page containing it arrives. Attachment URLs are pre-signed and expire
// within minutes, so downloads must happen here rather than after the whole
// thread has been fetched. An error aborts the thread fetch.
type EntryHook func(ctx context.Context, entry *models.Entry) error

// ThreadOption customises GetThread.
type ThreadOption func(*threadOptions)

type threadOptions struct {
	entryHook EntryHook
}

// WithAttachmentHook makes GetThread record each entry's uploaded attachments
// and pass the entry to hook so it can download them. Without this option
// attachments are ignored and the exported output is unchanged.
func WithAttachmentHook(hook EntryHook) ThreadOption {
	return func(o *threadOptions) { o.entryHook = hook }
}

// toAttachments converts attachment URLs into models. The file name is the
// last path segment of the URL.
func toAttachments(urls []string) []models.Attachment {
	var out []models.Attachment
	for _, raw := range urls {
		if raw == "" {
			continue
		}
		name := ""
		if u, err := url.Parse(raw); err == nil {
			name = path.Base(u.Path)
			if name == "." || name == "/" {
				name = ""
			}
		}
		out = append(out, models.Attachment{Name: name, URL: raw})
	}
	return out
}

// toEntry converts a raw API thread entry into the exported model.
func toEntry(e ThreadEntry) models.Entry {
	entry := models.Entry{
		UUID:        e.UUID,
		Query:       e.QueryStr,
		Model:       e.DisplayModel,
		SearchFocus: e.SearchFocus,
		CreatedAt:   parseTime(e.CreatedAt),
	}

	// Extract markdown answer from blocks (prefer ask_text_0_markdown over ask_text).
	for _, b := range e.Blocks {
		if b.MarkdownBlock != nil && b.IntendedUsage == "ask_text_0_markdown" && b.MarkdownBlock.Answer != "" {
			entry.Answer = b.MarkdownBlock.Answer
			break
		}
	}
	if entry.Answer == "" {
		for _, b := range e.Blocks {
			if b.MarkdownBlock != nil && b.IntendedUsage == "ask_text" && b.MarkdownBlock.Answer != "" {
				entry.Answer = b.MarkdownBlock.Answer
				break
			}
		}
	}

	// Extract sources from web_results block.
	for _, b := range e.Blocks {
		if b.WebResultBlock != nil && b.IntendedUsage == "web_results" {
			for _, wr := range b.WebResultBlock.WebResults {
				entry.Sources = append(entry.Sources, models.Source{
					Title:   wr.Name,
					URL:     wr.URL,
					Snippet: wr.Snippet,
				})
			}
			break
		}
	}

	return entry
}

// GetThread fetches the full detail of a single thread including all entries.
//
// The detail endpoint paginates by cursor: each response carries a next_cursor
// that is passed back to fetch the following page. If resume is a previously
// persisted incomplete thread, fetching continues from its NextCursor instead
// of restarting. onPage, when non-nil, is called after each fetched page so the
// caller can checkpoint progress; a page fetch failure leaves the last
// checkpoint on disk to be resumed by a later run.
func GetThread(ctx context.Context, c getter, uuid string, resume *models.Thread, onPage func(*models.Thread) error, opts ...ThreadOption) (*models.Thread, error) {
	var o threadOptions
	for _, opt := range opts {
		opt(&o)
	}
	// A checkpoint written without attachment export has entries whose
	// attachments were never collected, so it cannot be continued with them.
	if resume != nil && o.entryHook != nil && !resume.AttachmentsFetched {
		resume = nil
	}

	thread := &models.Thread{UUID: uuid, Slug: uuid}
	// AttachmentsFetched must only claim what this run (and the checkpoint it
	// resumes from) actually collected.
	thread.AttachmentsFetched = o.entryHook != nil && (resume == nil || resume.AttachmentsFetched)
	cursor := ""
	seen := make(map[string]bool)
	// seenCursors bounds the loop: the endpoint is reverse-engineered, so a
	// misbehaving response that repeats a cursor must not spin forever.
	seenCursors := make(map[string]bool)

	if resume != nil && !resume.Complete && resume.NextCursor != "" {
		thread.Title = resume.Title
		thread.CreatedAt = resume.CreatedAt
		thread.UpdatedAt = resume.UpdatedAt
		thread.SpaceUUID = resume.SpaceUUID
		thread.Bookmarked = resume.Bookmarked
		thread.Entries = append(thread.Entries, resume.Entries...)
		for _, e := range thread.Entries {
			seen[e.UUID] = true
		}
		cursor = resume.NextCursor
		seenCursors[cursor] = true
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var page ThreadDetailResponse
		if err := c.Get(ctx, threadPagePath(uuid, cursor), &page); err != nil {
			return nil, fmt.Errorf("failed to get thread %s page: %w", uuid, err)
		}

		// Metadata only comes back reliably on the first page of a run.
		if thread.Title == "" {
			thread.Title = page.ThreadMetadata.Title
		}
		if thread.CreatedAt.IsZero() {
			thread.CreatedAt = parseTime(page.ThreadMetadata.CreatedAt)
		}
		if thread.UpdatedAt.IsZero() {
			thread.UpdatedAt = parseTime(page.ThreadMetadata.UpdatedAt)
		}

		// A cursor boundary can re-include the last entry, so dedupe by UUID.
		for _, e := range page.Entries {
			if seen[e.UUID] {
				continue
			}
			seen[e.UUID] = true
			entry := toEntry(e)
			if o.entryHook != nil {
				entry.Attachments = toAttachments(e.Attachments)
				if err := o.entryHook(ctx, &entry); err != nil {
					return nil, fmt.Errorf("failed to process entry %s of thread %s: %w", e.UUID, uuid, err)
				}
			}
			thread.Entries = append(thread.Entries, entry)
		}

		// The thread ends when the API reports no further page or omits the
		// next cursor. Both are checked to guard against inconsistent responses.
		if !page.HasNextPage || page.NextCursor == nil || *page.NextCursor == "" {
			break
		}

		cursor = *page.NextCursor
		// A cursor that has already been used this run means the API is not
		// advancing. Stop with an error so the last good checkpoint on disk is
		// preserved for a later resume instead of looping or marking the
		// partially-fetched thread complete.
		if seenCursors[cursor] {
			return nil, fmt.Errorf("thread %s did not advance past cursor: possible API pagination loop", uuid)
		}
		seenCursors[cursor] = true

		thread.NextCursor = cursor
		if onPage != nil {
			if err := onPage(thread); err != nil {
				return nil, fmt.Errorf("failed to checkpoint thread %s: %w", uuid, err)
			}
		}
	}

	thread.Complete = true
	thread.NextCursor = ""
	return thread, nil
}

// parseTime attempts to parse a time string in multiple formats.
func parseTime(s string) time.Time {
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000000",
		"2006-01-02T15:04:05.000000+00:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}

	return time.Time{}
}
