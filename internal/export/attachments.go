package export

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/clappingmonkey/deplexity/internal/models"
)

// MaxAttachmentSize caps how large a single downloaded attachment may be.
// Attachments are held in memory while downloading, so this stays modest.
const MaxAttachmentSize int64 = 100 << 20 // 100 MB

// attachmentsDirName is the per-thread folder that holds downloaded attachments.
const attachmentsDirName = "attachments"

// RawURLGetter downloads an absolute URL with a size cap and no session
// credentials. It is satisfied by *client.Client.
type RawURLGetter interface {
	GetRawURLMax(ctx context.Context, rawURL string, maxBody int64) ([]byte, error)
}

// AttachmentDownloader saves the files a user uploaded with their queries into
// threads/<thread>/attachments/.
type AttachmentDownloader struct {
	OutputDir string
	Client    RawURLGetter
	// MaxSize overrides MaxAttachmentSize when positive.
	MaxSize int64
	// Warn receives one line per attachment that could not be downloaded.
	// It may be nil.
	Warn func(format string, args ...any)
}

// Hook returns a function that downloads every attachment of an entry that
// belongs to the given thread. Download failures are recorded on the
// attachment (and retried on the next run) instead of failing the thread; only
// cancellation is returned as an error.
func (d *AttachmentDownloader) Hook(slug, uuid string) func(context.Context, *models.Entry) error {
	dir := filepath.Join(d.OutputDir, "threads", threadDirName(slug, uuid), attachmentsDirName)
	return func(ctx context.Context, entry *models.Entry) error {
		for i := range entry.Attachments {
			if err := d.download(ctx, dir, &entry.Attachments[i]); err != nil {
				return err
			}
		}
		return nil
	}
}

func (d *AttachmentDownloader) download(ctx context.Context, dir string, a *models.Attachment) error {
	rawURL := a.URL
	a.URL = "" // never keep the pre-signed link, it carries credentials

	name := attachmentFileName(rawURL, a.Name)
	dst := filepath.Join(dir, name)

	// A previous run already stored this exact file; the name is derived from
	// the stable part of the URL, so it does not need downloading again.
	if info, err := os.Lstat(dst); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		a.File = path.Join(attachmentsDirName, name)
		a.Size = info.Size()
		return nil
	}

	if err := d.fetch(ctx, rawURL, dir, dst, a); err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		a.File, a.Size = "", 0
		a.Error = redactURL(err.Error(), rawURL)
		if d.Warn != nil {
			d.Warn("could not download attachment %q: %s", a.Name, a.Error)
		}
	}
	return nil
}

func (d *AttachmentDownloader) fetch(ctx context.Context, rawURL, dir, dst string, a *models.Attachment) error {
	limit := d.MaxSize
	if limit <= 0 {
		limit = MaxAttachmentSize
	}
	body, err := d.Client.GetRawURLMax(ctx, rawURL, limit)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("downloaded attachment is empty")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("could not create attachments directory: %w", err)
	}
	if err := writeFileAtomic(dst, body); err != nil {
		return err
	}
	a.File = path.Join(attachmentsDirName, filepath.Base(dst))
	a.Size = int64(len(body))
	a.Error = ""
	return nil
}

// writeFileAtomic publishes data at dst only once it is fully written, so an
// interrupted run never leaves a truncated file that looks complete.
func writeFileAtomic(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".attachment.tmp-*")
	if err != nil {
		return fmt.Errorf("could not create temporary attachment: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("could not write attachment: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("could not set attachment permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("could not close attachment: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("could not publish attachment: %w", err)
	}
	return nil
}

// attachmentFileName returns a collision-free, filesystem-safe file name. An
// 8-hex-digit hash of the URL path (which, unlike the query string, is stable
// across fetches) keeps equal original names from overwriting each other.
func attachmentFileName(rawURL, originalName string) string {
	urlPath := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		urlPath = u.Path
	}
	sum := sha256.Sum256([]byte(urlPath))
	prefix := fmt.Sprintf("%x", sum[:4])

	// Take the extension from the original name first: sanitizeFilename
	// truncates long names and would cut it off.
	ext := sanitizeFilename(path.Ext(originalName))
	if ext == "unnamed" || len(ext) > 16 {
		ext = ""
	} else if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	stem := "attachment"
	if trimmed := strings.TrimSuffix(originalName, path.Ext(originalName)); strings.TrimSpace(trimmed) != "" {
		stem = sanitizeFilename(trimmed)
	}
	const maxStem = 100
	if len(stem) > maxStem {
		stem = stem[:maxStem]
	}
	return prefix + "-" + stem + ext
}

// redactURL removes a pre-signed URL (which embeds credentials in its query
// string) from an error message so it is never written to thread.json or the
// manifest.
func redactURL(msg, rawURL string) string {
	if rawURL == "" {
		return msg
	}
	clean := rawURL
	if u, err := url.Parse(rawURL); err == nil {
		u.RawQuery = ""
		u.Fragment = ""
		clean = u.String()
	}
	return strings.ReplaceAll(msg, rawURL, clean)
}

// AttachmentsPresent reports whether every attachment recorded on the cached
// thread is still on disk with the recorded size, so a run can skip refetching
// it. A thread without attachments is trivially present. Failed attachments are
// never "present", which makes the next run retry them.
func (e *JSONExporter) AttachmentsPresent(thread *models.Thread) bool {
	dir := e.threadDir(thread)
	for _, entry := range thread.Entries {
		for _, a := range entry.Attachments {
			if a.Error != "" || a.File == "" {
				return false
			}
			info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(a.File)))
			if err != nil || !info.Mode().IsRegular() || info.Size() != a.Size {
				return false
			}
		}
	}
	return true
}

// copyThreadAttachments mirrors a thread's downloaded attachments into a space
// folder so the space stays self-contained, like the thread PDF copy. Files that
// are already there with the same size are left alone.
func copyThreadAttachments(ctx context.Context, outputDir, dstThreadDir string, thread *models.Thread) error {
	srcDir := filepath.Join(outputDir, "threads", threadDirName(thread.Slug, thread.UUID))
	for _, entry := range thread.Entries {
		for _, a := range entry.Attachments {
			if a.File == "" {
				continue
			}
			rel := filepath.FromSlash(a.File)
			src := filepath.Join(srcDir, rel)
			dst := filepath.Join(dstThreadDir, rel)
			if info, err := os.Lstat(dst); err == nil && info.Mode().IsRegular() && info.Size() == a.Size {
				continue
			}
			if err := copyFileAtomic(ctx, src, dst); err != nil {
				return fmt.Errorf("could not copy attachment %q into space: %w", a.Name, err)
			}
		}
	}
	return nil
}

// copyFileAtomic copies a regular file and publishes the copy only once it is
// complete, so an interrupted copy never leaves a truncated attachment behind.
// It stops early when ctx is cancelled.
func copyFileAtomic(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("could not open source file: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("could not inspect source file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", src)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("could not create directory for copy: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".attachment.tmp-*")
	if err != nil {
		return fmt.Errorf("could not create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			tmp.Close()
			return err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, err := tmp.Write(buf[:n]); err != nil {
				tmp.Close()
				return fmt.Errorf("could not copy file: %w", err)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmp.Close()
			return fmt.Errorf("could not read source file: %w", readErr)
		}
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("could not set file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("could not close copy: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return fmt.Errorf("could not publish copy: %w", err)
	}
	return nil
}
