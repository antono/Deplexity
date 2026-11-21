package export

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clappingmonkey/deplexity/internal/models"
)

type fakeGetter struct {
	bodies map[string][]byte
	errs   map[string]error
	calls  []string
	limits []int64
}

func (f *fakeGetter) GetRawURLMax(_ context.Context, rawURL string, maxBody int64) ([]byte, error) {
	f.calls = append(f.calls, rawURL)
	f.limits = append(f.limits, maxBody)
	if err := f.errs[rawURL]; err != nil {
		return nil, err
	}
	body, ok := f.bodies[rawURL]
	if !ok {
		return nil, errors.New("not found")
	}
	return body, nil
}

const (
	attURLA = "https://files.example.com/web/direct-files/attachments/images/137/aaaa/photo.jpg?X-Amz-Signature=secret-a"
	attURLB = "https://files.example.com/web/direct-files/attachments/images/137/bbbb/photo.jpg?X-Amz-Signature=secret-b"
)

func newDownloader(t *testing.T, g *fakeGetter) (*AttachmentDownloader, string) {
	t.Helper()
	dir := t.TempDir()
	return &AttachmentDownloader{OutputDir: dir, Client: g}, dir
}

func TestAttachmentHookDownloadsIntoThreadDirectory(t *testing.T) {
	g := &fakeGetter{bodies: map[string][]byte{attURLA: []byte("jpeg-bytes-a"), attURLB: []byte("jpeg-bytes-bb")}}
	d, out := newDownloader(t, g)

	entry := &models.Entry{UUID: "e1", Attachments: []models.Attachment{
		{Name: "photo.jpg", URL: attURLA},
		{Name: "photo.jpg", URL: attURLB}, // same name, different file
	}}
	if err := d.Hook("my-thread", "thread-1")(context.Background(), entry); err != nil {
		t.Fatalf("hook: %v", err)
	}

	dir := filepath.Join(out, "threads", threadDirName("my-thread", "thread-1"))
	seen := map[string]bool{}
	for i, want := range []string{"jpeg-bytes-a", "jpeg-bytes-bb"} {
		a := entry.Attachments[i]
		if a.Error != "" || a.File == "" || a.URL != "" {
			t.Fatalf("attachment %d = %+v, want a downloaded file and no stored URL", i, a)
		}
		if !strings.HasPrefix(a.File, "attachments/") || !strings.HasSuffix(a.File, "-photo.jpg") {
			t.Errorf("attachment %d file = %q, want attachments/<hash>-photo.jpg", i, a.File)
		}
		if seen[a.File] {
			t.Errorf("attachments share the file name %q and would overwrite each other", a.File)
		}
		seen[a.File] = true
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(a.File)))
		if err != nil || string(data) != want {
			t.Errorf("attachment %d content = %q, %v; want %q", i, data, err, want)
		}
		if a.Size != int64(len(want)) {
			t.Errorf("attachment %d size = %d, want %d", i, a.Size, len(want))
		}
	}
	for _, limit := range g.limits {
		if limit != MaxAttachmentSize {
			t.Errorf("download cap = %d, want %d", limit, MaxAttachmentSize)
		}
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "attachments", ".attachment.tmp-*"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

func TestAttachmentHookRecordsFailureWithoutLeakingCredentials(t *testing.T) {
	g := &fakeGetter{errs: map[string]error{
		attURLA: errors.New("unexpected response from " + attURLA + ": HTTP 403"),
	}, bodies: map[string][]byte{attURLB: []byte("ok")}}
	d, _ := newDownloader(t, g)
	var warnings []string
	d.Warn = func(format string, args ...any) { warnings = append(warnings, format) }

	entry := &models.Entry{Attachments: []models.Attachment{
		{Name: "photo.jpg", URL: attURLA},
		{Name: "photo.jpg", URL: attURLB},
	}}
	if err := d.Hook("", "thread-1")(context.Background(), entry); err != nil {
		t.Fatalf("a failed download must not fail the thread: %v", err)
	}

	failed := entry.Attachments[0]
	if failed.File != "" || failed.Error == "" {
		t.Fatalf("failed attachment = %+v, want an error and no file", failed)
	}
	if strings.Contains(failed.Error, "secret-a") || strings.Contains(failed.Error, "X-Amz") {
		t.Errorf("error %q leaks the pre-signed URL", failed.Error)
	}
	if !strings.Contains(failed.Error, "HTTP 403") {
		t.Errorf("error %q lost the failure reason", failed.Error)
	}
	if entry.Attachments[1].File == "" {
		t.Error("a later attachment was skipped after an earlier failure")
	}
	if len(warnings) != 1 {
		t.Errorf("got %d warnings, want 1", len(warnings))
	}
}

func TestAttachmentHookPropagatesCancellation(t *testing.T) {
	g := &fakeGetter{errs: map[string]error{attURLA: context.Canceled}}
	d, _ := newDownloader(t, g)
	entry := &models.Entry{Attachments: []models.Attachment{{Name: "photo.jpg", URL: attURLA}, {Name: "photo.jpg", URL: attURLB}}}

	if err := d.Hook("", "thread-1")(context.Background(), entry); !errors.Is(err, context.Canceled) {
		t.Fatalf("hook error = %v, want cancellation to abort the export", err)
	}
	if len(g.calls) != 1 {
		t.Errorf("made %d downloads after cancellation, want 1", len(g.calls))
	}
}

func TestAttachmentHookSkipsFilesAlreadyOnDisk(t *testing.T) {
	g := &fakeGetter{bodies: map[string][]byte{attURLA: []byte("first")}}
	d, _ := newDownloader(t, g)
	hook := d.Hook("", "thread-1")

	first := &models.Entry{Attachments: []models.Attachment{{Name: "photo.jpg", URL: attURLA}}}
	if err := hook(context.Background(), first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A later run gets a freshly signed URL for the same file.
	refreshed := strings.Replace(attURLA, "secret-a", "secret-new", 1)
	second := &models.Entry{Attachments: []models.Attachment{{Name: "photo.jpg", URL: refreshed}}}
	if err := hook(context.Background(), second); err != nil {
		t.Fatalf("second run: %v", err)
	}

	if len(g.calls) != 1 {
		t.Errorf("downloaded %d times, want 1 (the second run reuses the file)", len(g.calls))
	}
	if first.Attachments[0].File != second.Attachments[0].File || second.Attachments[0].Size != 5 {
		t.Errorf("second = %+v, want the same file and size as the first %+v", second.Attachments[0], first.Attachments[0])
	}
}

func TestAttachmentHookRejectsEmptyDownload(t *testing.T) {
	g := &fakeGetter{bodies: map[string][]byte{attURLA: {}}}
	d, _ := newDownloader(t, g)
	entry := &models.Entry{Attachments: []models.Attachment{{Name: "photo.jpg", URL: attURLA}}}

	if err := d.Hook("", "thread-1")(context.Background(), entry); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if a := entry.Attachments[0]; a.File != "" || a.Error == "" {
		t.Errorf("attachment = %+v, want an empty download recorded as failed", a)
	}
}

func TestAttachmentFileNameIsSafeAndStable(t *testing.T) {
	a := attachmentFileName(attURLA, "../../etc/Pass Wd.TXT")
	if strings.ContainsAny(a, "/\\ ") || filepath.Base(a) != a {
		t.Errorf("file name %q is not a single safe path component", a)
	}
	if !strings.HasSuffix(a, ".txt") {
		t.Errorf("file name %q lost its extension", a)
	}
	if b := attachmentFileName(strings.Replace(attURLA, "secret-a", "other", 1), "../../etc/Pass Wd.TXT"); a != b {
		t.Errorf("name changed with the query string: %q vs %q", a, b)
	}
	if attachmentFileName(attURLA, "photo.jpg") == attachmentFileName(attURLB, "photo.jpg") {
		t.Error("different files with the same name collide")
	}
	if got := attachmentFileName(attURLA, ""); !strings.HasSuffix(got, "-attachment") {
		t.Errorf("unnamed attachment = %q, want a placeholder name", got)
	}
	long := attachmentFileName(attURLA, strings.Repeat("x", 500)+".pdf")
	if len(long) > 128 || !strings.HasSuffix(long, ".pdf") {
		t.Errorf("long name = %q (%d bytes), want a truncated name that keeps .pdf", long, len(long))
	}
}

func TestAttachmentsPresentDetectsMissingAndFailedFiles(t *testing.T) {
	g := &fakeGetter{bodies: map[string][]byte{attURLA: []byte("data")}}
	d, out := newDownloader(t, g)
	exp := &JSONExporter{OutputDir: out}

	thread := &models.Thread{UUID: "thread-1", Slug: "thread-1", Entries: []models.Entry{{Attachments: []models.Attachment{{Name: "photo.jpg", URL: attURLA}}}}}
	if !exp.AttachmentsPresent(&models.Thread{UUID: "thread-1", Entries: []models.Entry{{}}}) {
		t.Error("a thread without attachments must count as present")
	}
	if err := d.Hook("", "thread-1")(context.Background(), &thread.Entries[0]); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !exp.AttachmentsPresent(thread) {
		t.Fatal("downloaded attachment reported missing")
	}

	file := filepath.Join(out, "threads", threadDirName("thread-1", "thread-1"), filepath.FromSlash(thread.Entries[0].Attachments[0].File))
	if err := os.WriteFile(file, []byte("truncated-by-hand-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exp.AttachmentsPresent(thread) {
		t.Error("a file with a different size must count as missing")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if exp.AttachmentsPresent(thread) {
		t.Error("a deleted file must count as missing")
	}

	failed := &models.Thread{UUID: "thread-1", Entries: []models.Entry{{Attachments: []models.Attachment{{Name: "x", Error: "HTTP 403"}}}}}
	if exp.AttachmentsPresent(failed) {
		t.Error("a failed attachment must be retried, not treated as present")
	}
}

func TestExportSpacesCopiesAttachmentsIntoSpaceFolder(t *testing.T) {
	g := &fakeGetter{bodies: map[string][]byte{attURLA: []byte("space-data")}}
	d, out := newDownloader(t, g)

	thread := models.Thread{UUID: "thread-1", Slug: "chat", Complete: true, Entries: []models.Entry{{UUID: "e1", Query: "q", Answer: "a", Attachments: []models.Attachment{{Name: "photo.jpg", URL: attURLA}}}}}
	if err := d.Hook("chat", "thread-1")(context.Background(), &thread.Entries[0]); err != nil {
		t.Fatalf("hook: %v", err)
	}
	spaces := []models.Space{{UUID: "space-1", Name: "Work", ThreadUUIDs: []string{"thread-1"}}}

	for _, run := range []func() error{
		func() error {
			return (&JSONExporter{OutputDir: out}).ExportSpaces(context.Background(), spaces, []models.Thread{thread})
		},
		func() error {
			return (&MarkdownExporter{OutputDir: out}).ExportSpaces(context.Background(), spaces, []models.Thread{thread})
		},
	} {
		if err := run(); err != nil {
			t.Fatalf("ExportSpaces: %v", err)
		}
	}

	spaceDir := filepath.Join(out, "spaces", spaceDirNames(spaces)[0], "threads", threadDirName("chat", "thread-1"))
	data, err := os.ReadFile(filepath.Join(spaceDir, filepath.FromSlash(thread.Entries[0].Attachments[0].File)))
	if err != nil || string(data) != "space-data" {
		t.Fatalf("space copy = %q, %v; want the attachment copied next to the space thread", data, err)
	}
	md, err := os.ReadFile(filepath.Join(spaceDir, "thread.md"))
	if err != nil || !strings.Contains(string(md), "](attachments/") {
		t.Errorf("space thread.md = %q, %v; want a link to the copied attachment", md, err)
	}
}

func TestThreadMarkdownListsAttachments(t *testing.T) {
	dir := t.TempDir()
	thread := &models.Thread{UUID: "thread-1", Entries: []models.Entry{{
		Query:  "what is in these?",
		Answer: "answer",
		Attachments: []models.Attachment{
			{Name: "my photo.jpg", File: "attachments/ab12cd34-my-photo.jpg", Size: 137},
			{Name: "broken.pdf", Error: "HTTP 403"},
		},
	}}}
	if err := writeThreadMarkdown(dir, thread); err != nil {
		t.Fatalf("writeThreadMarkdown: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "thread.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(data)
	for _, want := range []string{
		"### Attachments",
		"- [my photo.jpg](attachments/ab12cd34-my-photo.jpg) (137 bytes)",
		"- broken.pdf — not downloaded: HTTP 403",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("thread.md is missing %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "### Attachments") > strings.Index(md, "answer") {
		t.Error("attachments should appear before the answer, next to the question")
	}
}

func TestThreadMarkdownWithoutAttachmentsIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	thread := &models.Thread{UUID: "thread-1", Entries: []models.Entry{{Query: "q", Answer: "a"}}}
	if err := writeThreadMarkdown(dir, thread); err != nil {
		t.Fatalf("writeThreadMarkdown: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "thread.md"))
	if strings.Contains(string(data), "Attachments") {
		t.Errorf("thread.md mentions attachments for a thread without any:\n%s", data)
	}
}
