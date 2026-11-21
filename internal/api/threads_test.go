package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/clappingmonkey/deplexity/internal/models"
)

type threadGetter struct {
	responses []ThreadDetailResponse
	calls     int
	paths     []string
}

type threadPoster struct {
	responses []ThreadListResponse
	errors    []error
	calls     int
	offsets   []int
}

func (p *threadPoster) Post(_ context.Context, _ string, body any, dst any) error {
	req := body.(ThreadListRequest)
	p.offsets = append(p.offsets, req.Offset)
	if p.calls >= len(p.responses) {
		return errors.New("unexpected list request")
	}
	call := p.calls
	p.calls++
	if call < len(p.errors) && p.errors[call] != nil {
		return p.errors[call]
	}
	*dst.(*ThreadListResponse) = p.responses[call]
	return nil
}

func (g *threadGetter) Get(_ context.Context, path string, dst any) error {
	g.paths = append(g.paths, path)
	if g.calls >= len(g.responses) {
		return errors.New("rate limited")
	}
	*dst.(*ThreadDetailResponse) = g.responses[g.calls]
	g.calls++
	return nil
}

func TestParseTime(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			name:  "RFC3339",
			input: "2024-06-15T10:30:00Z",
			want:  time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:  "RFC3339Nano",
			input: "2024-06-15T10:30:00.123456789Z",
			want:  time.Date(2024, 6, 15, 10, 30, 0, 123456789, time.UTC),
		},
		{
			name:  "with milliseconds",
			input: "2024-06-15T10:30:00.000Z",
			want:  time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:  "space separated",
			input: "2024-06-15 10:30:00",
			want:  time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			name:  "empty string",
			input: "",
			want:  time.Time{},
		},
		{
			name:  "garbage",
			input: "not-a-date",
			want:  time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTime(tt.input)
			if !got.Equal(tt.want) {
				t.Errorf("parseTime(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestThreadListItemMapping(t *testing.T) {
	raw := ThreadListItem{
		UUID:   "abc-123",
		Title:  "Test Thread",
		Slug:   "abc-123",
		Status: "completed",
	}

	if raw.UUID != "abc-123" {
		t.Errorf("unexpected UUID: %s", raw.UUID)
	}
	if raw.Title != "Test Thread" {
		t.Errorf("unexpected Title: %s", raw.Title)
	}
}

func TestListThreadsStartsAtZeroAndAdvancesByPageLength(t *testing.T) {
	first := make(ThreadListResponse, 20)
	for i := range first {
		first[i] = ThreadListItem{UUID: fmt.Sprintf("thread-%d", i)}
	}
	poster := &threadPoster{responses: []ThreadListResponse{
		first,
		{{UUID: "thread-20"}},
	}}
	var progress []int

	threads, err := ListThreads(context.Background(), poster, func(n int) {
		progress = append(progress, n)
	})
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 21 {
		t.Fatalf("got %d threads, want 21", len(threads))
	}
	if !reflect.DeepEqual(poster.offsets, []int{0, 20}) {
		t.Errorf("offsets = %v, want [0 20]", poster.offsets)
	}
	if !reflect.DeepEqual(progress, []int{20, 21}) {
		t.Errorf("progress = %v, want [20 21]", progress)
	}
}

func TestListThreadsStopsOnCurrentRunDuplicatePage(t *testing.T) {
	first := make(ThreadListResponse, 20)
	for i := range first {
		first[i] = ThreadListItem{UUID: fmt.Sprintf("thread-%d", i)}
	}
	duplicate := append(ThreadListResponse(nil), first...)
	poster := &threadPoster{responses: []ThreadListResponse{first, duplicate}}

	threads, err := ListThreads(context.Background(), poster, nil)
	if err != nil {
		t.Fatalf("ListThreads: %v", err)
	}
	if len(threads) != 20 {
		t.Fatalf("got %d threads, want 20 unique threads", len(threads))
	}
	if poster.calls != 2 {
		t.Fatalf("calls = %d, want 2", poster.calls)
	}
}

func TestListThreadsReturnsPartialResultsOnPageError(t *testing.T) {
	first := make(ThreadListResponse, 20)
	for i := range first {
		first[i] = ThreadListItem{UUID: fmt.Sprintf("thread-%d", i)}
	}
	wantErr := errors.New("second page failed")
	poster := &threadPoster{
		responses: []ThreadListResponse{first, nil},
		errors:    []error{nil, wantErr},
	}

	threads, err := ListThreads(context.Background(), poster, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want second page failure", err)
	}
	if len(threads) != 20 {
		t.Fatalf("got %d partial threads, want 20", len(threads))
	}
}

func cursorPtr(s string) *string { return &s }

func TestThreadPagePathEscapesCursor(t *testing.T) {
	cursor := `{"M":{"entry_uuid":{"S":"abc"}}}`
	path := threadPagePath("thread-1", cursor)

	if !strings.Contains(path, "offset=0") {
		t.Errorf("path = %q, want offset pinned to 0", path)
	}
	if !strings.Contains(path, "limit=100") {
		t.Errorf("path = %q, want limit=100", path)
	}
	if !strings.Contains(path, "cursor=%7B%22M%22") {
		t.Errorf("path = %q, want URL-escaped cursor", path)
	}
	if strings.Contains(path, cursor) {
		t.Errorf("path = %q, want the raw cursor JSON escaped", path)
	}
}

func TestThreadPagePathOmitsEmptyCursor(t *testing.T) {
	if path := threadPagePath("thread-1", ""); strings.Contains(path, "cursor=") {
		t.Errorf("path = %q, want no cursor param on the first page", path)
	}
}

func TestGetThreadPaginatesByCursor(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{
		{
			ThreadMetadata: ThreadMetadata{Title: "Long thread"},
			Entries:        []ThreadEntry{{UUID: "entry-1"}, {UUID: "entry-2"}},
			HasNextPage:    true,
			NextCursor:     cursorPtr("cursor-1"),
		},
		{
			// A cursor boundary re-includes the last entry of the prior page.
			Entries:     []ThreadEntry{{UUID: "entry-2"}, {UUID: "entry-3"}},
			HasNextPage: false,
			NextCursor:  cursorPtr(""),
		},
	}}

	thread, err := GetThread(context.Background(), getter, "thread-1", nil, nil)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if getter.calls != 2 {
		t.Errorf("made %d requests, want 2", getter.calls)
	}
	if strings.Contains(getter.paths[0], "cursor=") {
		t.Errorf("first request = %q, want no cursor", getter.paths[0])
	}
	if !strings.Contains(getter.paths[1], "cursor=cursor-1") {
		t.Errorf("second request = %q, want cursor from first page", getter.paths[1])
	}

	want := []string{"entry-1", "entry-2", "entry-3"}
	if len(thread.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(thread.Entries), len(want), thread.Entries)
	}
	for i, uuid := range want {
		if thread.Entries[i].UUID != uuid {
			t.Errorf("entry %d = %q, want %q", i, thread.Entries[i].UUID, uuid)
		}
	}
	if !thread.Complete {
		t.Error("completed thread not marked complete")
	}
	if thread.NextCursor != "" {
		t.Errorf("NextCursor = %q, want cleared on completion", thread.NextCursor)
	}
}

func TestGetThreadStopsWhenCursorRepeats(t *testing.T) {
	// A reverse-engineered endpoint could return the same cursor forever;
	// the fetch must terminate with an error rather than spinning, and must
	// not mark the partially-fetched thread complete.
	repeating := ThreadDetailResponse{
		ThreadMetadata: ThreadMetadata{Title: "Long thread"},
		Entries:        []ThreadEntry{{UUID: "entry-1"}},
		HasNextPage:    true,
		NextCursor:     cursorPtr("cursor-1"),
	}
	responses := make([]ThreadDetailResponse, 10)
	for i := range responses {
		responses[i] = repeating
	}
	getter := &threadGetter{responses: responses}

	thread, err := GetThread(context.Background(), getter, "thread-1", nil, nil)
	if err == nil {
		t.Fatal("GetThread succeeded despite a non-advancing cursor")
	}
	if thread != nil {
		t.Fatalf("GetThread returned a thread: %+v", thread)
	}
	if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("error = %q, want cursor-loop context", err)
	}
	// Page 1 (no cursor) + page 2 (cursor-1) is enough to detect the repeat.
	if getter.calls > 2 {
		t.Errorf("made %d requests, want stop after detecting the repeat", getter.calls)
	}
}

func TestGetThreadReturnsErrorWhenLaterPageFails(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		ThreadMetadata: ThreadMetadata{Title: "Long thread"},
		Entries:        []ThreadEntry{{UUID: "entry-1"}},
		HasNextPage:    true,
		NextCursor:     cursorPtr("cursor-1"),
	}}}

	thread, err := GetThread(context.Background(), getter, "thread-1", nil, nil)
	if err == nil {
		t.Fatal("GetThread succeeded after a later page failed")
	}
	if thread != nil {
		t.Fatalf("GetThread returned a partial thread: %+v", thread)
	}
	if !strings.Contains(err.Error(), "thread-1") {
		t.Errorf("error = %q, want thread context", err)
	}
}

func TestGetThreadCheckpointsProgressBeforeFailure(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		ThreadMetadata: ThreadMetadata{Title: "Long thread"},
		Entries:        []ThreadEntry{{UUID: "entry-1"}, {UUID: "entry-2"}},
		HasNextPage:    true,
		NextCursor:     cursorPtr("cursor-1"),
	}}}

	var checkpoints []models.Thread
	_, err := GetThread(context.Background(), getter, "thread-1", nil, func(t *models.Thread) error {
		checkpoints = append(checkpoints, *t)
		return nil
	})
	if err == nil {
		t.Fatal("GetThread succeeded after a later page failed")
	}
	if len(checkpoints) != 1 {
		t.Fatalf("got %d checkpoints, want 1", len(checkpoints))
	}
	cp := checkpoints[0]
	if cp.Complete {
		t.Error("checkpoint marked complete before the fetch finished")
	}
	if cp.NextCursor != "cursor-1" {
		t.Errorf("checkpoint NextCursor = %q, want cursor-1", cp.NextCursor)
	}
	if len(cp.Entries) != 2 {
		t.Errorf("checkpoint has %d entries, want 2", len(cp.Entries))
	}
	if cp.Title != "Long thread" {
		t.Errorf("checkpoint Title = %q, want metadata preserved", cp.Title)
	}
}

func TestGetThreadResumesFromCheckpoint(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		ThreadMetadata: ThreadMetadata{Title: "Long thread"},
		// The cursor boundary re-serves entry-2.
		Entries:     []ThreadEntry{{UUID: "entry-2"}, {UUID: "entry-3"}},
		HasNextPage: false,
		NextCursor:  cursorPtr(""),
	}}}

	resume := &models.Thread{
		UUID:       "thread-1",
		Title:      "Long thread",
		Entries:    []models.Entry{{UUID: "entry-1"}, {UUID: "entry-2"}},
		NextCursor: "cursor-1",
	}

	thread, err := GetThread(context.Background(), getter, "thread-1", resume, nil)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if getter.calls != 1 {
		t.Errorf("made %d requests, want 1 (no refetch from the start)", getter.calls)
	}
	if !strings.Contains(getter.paths[0], "cursor=cursor-1") {
		t.Errorf("first request = %q, want resume at cursor-1", getter.paths[0])
	}

	want := []string{"entry-1", "entry-2", "entry-3"}
	if len(thread.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(thread.Entries), len(want), thread.Entries)
	}
	for i, uuid := range want {
		if thread.Entries[i].UUID != uuid {
			t.Errorf("entry %d = %q, want %q", i, thread.Entries[i].UUID, uuid)
		}
	}
	if !thread.Complete {
		t.Error("completed thread not marked complete")
	}
	if thread.NextCursor != "" {
		t.Errorf("NextCursor = %q, want cleared on completion", thread.NextCursor)
	}
}

func TestGetThreadHonorsCancellation(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		ThreadMetadata: ThreadMetadata{Title: "Long thread"},
		Entries:        []ThreadEntry{{UUID: "entry-1"}},
		HasNextPage:    true,
		NextCursor:     cursorPtr("cursor-1"),
	}}}

	ctx, cancel := context.WithCancel(context.Background())
	_, err := GetThread(ctx, getter, "thread-1", nil, func(*models.Thread) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func attachmentEntry(uuid string, urls ...string) ThreadEntry {
	return ThreadEntry{UUID: uuid, QueryStr: "q " + uuid, Attachments: urls}
}

func TestGetThreadIgnoresAttachmentsWithoutHook(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		Entries:    []ThreadEntry{attachmentEntry("entry-1", "https://files.example.com/a/b/photo.jpg?X-Amz-Signature=secret")},
		NextCursor: cursorPtr(""),
	}}}

	thread, err := GetThread(context.Background(), getter, "thread-1", nil, nil)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if len(thread.Entries[0].Attachments) != 0 {
		t.Errorf("attachments = %+v, want none when the option is off", thread.Entries[0].Attachments)
	}
	if thread.AttachmentsFetched {
		t.Error("AttachmentsFetched = true without the hook")
	}
}

func TestGetThreadPassesNewEntriesToAttachmentHook(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{
		{
			Entries:     []ThreadEntry{attachmentEntry("entry-1", "https://files.example.com/a/b/photo%20one.jpg?X-Amz-Signature=secret")},
			HasNextPage: true,
			NextCursor:  cursorPtr("cursor-1"),
		},
		{
			// The cursor boundary re-serves entry-1; it must not be processed twice.
			Entries:    []ThreadEntry{attachmentEntry("entry-1", "https://files.example.com/a/b/photo%20one.jpg?X-Amz-Signature=other"), attachmentEntry("entry-2"), attachmentEntry("entry-3", "https://files.example.com/c/d/notes.pdf?x=1", "")},
			NextCursor: cursorPtr(""),
		},
	}}

	var seen []string
	hook := func(_ context.Context, e *models.Entry) error {
		for _, a := range e.Attachments {
			seen = append(seen, e.UUID+":"+a.Name+":"+a.URL)
		}
		return nil
	}
	thread, err := GetThread(context.Background(), getter, "thread-1", nil, nil, WithAttachmentHook(hook))
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}

	want := []string{
		"entry-1:photo one.jpg:https://files.example.com/a/b/photo%20one.jpg?X-Amz-Signature=secret",
		"entry-3:notes.pdf:https://files.example.com/c/d/notes.pdf?x=1",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("hook saw %v, want %v", seen, want)
	}
	if len(thread.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(thread.Entries))
	}
	if !thread.AttachmentsFetched {
		t.Error("AttachmentsFetched = false, want true when the hook ran")
	}
}

func TestGetThreadAbortsWhenAttachmentHookFails(t *testing.T) {
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		Entries:    []ThreadEntry{attachmentEntry("entry-1", "https://files.example.com/a/b/photo.jpg")},
		NextCursor: cursorPtr(""),
	}}}
	boom := errors.New("cancelled")
	hook := func(context.Context, *models.Entry) error { return boom }

	if _, err := GetThread(context.Background(), getter, "thread-1", nil, nil, WithAttachmentHook(hook)); !errors.Is(err, boom) {
		t.Fatalf("GetThread error = %v, want hook error", err)
	}
}

func TestGetThreadRestartsCheckpointWithoutAttachments(t *testing.T) {
	// The checkpoint was written by a run without attachment export, so its
	// entries never had their attachments collected; resuming would lose them.
	getter := &threadGetter{responses: []ThreadDetailResponse{{
		Entries:    []ThreadEntry{attachmentEntry("entry-1", "https://files.example.com/a/b/photo.jpg")},
		NextCursor: cursorPtr(""),
	}}}
	resume := &models.Thread{UUID: "thread-1", Entries: []models.Entry{{UUID: "entry-1"}}, NextCursor: "cursor-1"}
	hook := func(context.Context, *models.Entry) error { return nil }

	thread, err := GetThread(context.Background(), getter, "thread-1", resume, nil, WithAttachmentHook(hook))
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if strings.Contains(getter.paths[0], "cursor=") {
		t.Errorf("first request = %q, want a restart from the first page", getter.paths[0])
	}
	if len(thread.Entries[0].Attachments) != 1 {
		t.Errorf("attachments = %+v, want the entry's attachment collected", thread.Entries[0].Attachments)
	}
}

func TestGetThreadResumeKeepsAttachmentsFetchedOnlyWhenCheckpointHadThem(t *testing.T) {
	page := ThreadDetailResponse{Entries: []ThreadEntry{{UUID: "entry-2"}}, NextCursor: cursorPtr("")}
	resume := &models.Thread{UUID: "thread-1", Entries: []models.Entry{{UUID: "entry-1"}}, NextCursor: "cursor-1", AttachmentsFetched: true}
	hook := func(context.Context, *models.Entry) error { return nil }

	// With the hook, resuming a checkpoint that had attachments continues from the cursor.
	getter := &threadGetter{responses: []ThreadDetailResponse{page}}
	thread, err := GetThread(context.Background(), getter, "thread-1", resume, nil, WithAttachmentHook(hook))
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if !strings.Contains(getter.paths[0], "cursor=cursor-1") || !thread.AttachmentsFetched {
		t.Errorf("path = %q, fetched = %t, want resume at cursor-1 with attachments", getter.paths[0], thread.AttachmentsFetched)
	}

	// Without the hook the new entries have no attachments, so the flag must be dropped.
	getter = &threadGetter{responses: []ThreadDetailResponse{page}}
	thread, err = GetThread(context.Background(), getter, "thread-1", resume, nil)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.AttachmentsFetched {
		t.Error("AttachmentsFetched = true after a run without the hook")
	}
}
