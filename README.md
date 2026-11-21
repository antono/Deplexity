# Deplexity

**Export your Perplexity AI conversations, spaces, and profile — fully offline, no API key required.**

Deplexity is a CLI tool that authenticates via your browser session and exports all your Perplexity AI data into portable formats (JSON, Markdown, PDF). Built in pure Go with zero runtime dependencies.

> ## About this fork
>
> This is a **hard fork** of [clappingmonkey/Deplexity](https://github.com/clappingmonkey/Deplexity). It starts from upstream commit [`b436a58`](https://github.com/clappingmonkey/Deplexity/commit/b436a58) (v0.3.0 plus ten commits), the last one before upstream PR [#75 "preserve thread slugs"](https://github.com/clappingmonkey/Deplexity/pull/75), and does not include that change or anything built on top of it.
>
> **Why.** In the fork maintainer's opinion, #75 made a bad trade-off. It moved each thread from `threads/<uuid>/` to `threads/<slug>-<uuid-prefix>-<sha256>/`, and that has real costs for anyone keeping an archive:
>
> - The directory name is long and no more readable than a UUID, because of the hash suffix.
> - The name depends on a slug that Perplexity can change, so a thread can move to a new directory later.
> - Existing archives are not migrated. Old directories are deliberately kept next to the new ones, so upgrading re-downloads every thread and leaves a full duplicate of the archive, and a slug change leaves another stale copy behind.
> - For an archive that lives in git or is indexed by a search tool, that duplication is clutter that has to be cleaned up by hand.
>
> This fork keeps the original layout: one stable, never-moving `threads/<uuid>/` per thread, compatible with archives made before #75.
>
> **What differs from upstream**
>
> - Adds `--attachments` to download files you uploaded to threads (see [Uploaded attachments](#uploaded-attachments)).
> - Includes upstream fixes [#76](https://github.com/clappingmonkey/Deplexity/pull/76) (cancellable request pacing), [#79](https://github.com/clappingmonkey/Deplexity/pull/79) (no extra backoff after the last retry) and [#86](https://github.com/clappingmonkey/Deplexity/pull/86) (require session user identity).
> - Does not include #75, and therefore no upstream release from v0.4.0 on. Two later upstream changes cannot be applied on this base because they build on the new code: #80 (interruptible PDF rendering) and #108 (incremental PDF export).
> - Upstream #81 (exported bookmark count) and #84 (remove stale source sidecars) are independent of the layout but are not merged yet. Note that #81 also adds a cache version marker, which would make archives created before it be downloaded once more.
>
> Upstream changes are merged only when they do not require the new layout.

<p align="center">
  <img src="demo.gif" alt="Deplexity demo — exporting Perplexity threads to JSON, Markdown, and PDF" width="800">
</p>

---

## Why Deplexity?

Perplexity AI has no official data export feature. Your research threads, curated spaces, and citation-rich answers are locked inside their platform. Deplexity gives you:

- **Full ownership** of your data in open formats
- **Offline archives** you can search, version-control, or feed into other tools
- **PDF reports** with source attribution — no browser or LaTeX needed
- **Incremental backups** — resumable exports pick up where they left off

---

## Installation

### From source (requires Go 1.26+)

```bash
go install github.com/clappingmonkey/deplexity/cmd/deplexity@latest
```

### From source with Bazel

```bash
bazel build //cmd/deplexity
# Binary at: bazel-bin/cmd/deplexity/deplexity_/deplexity
```

### Pre-built binaries

Download from [Releases](https://github.com/clappingmonkey/deplexity/releases).

---

## Quick Start

```bash
# 1. Authenticate (opens browser — log in to Perplexity)
deplexity login

# 2. Export everything
deplexity export

# 3. Check your data
ls deplexity-export/
```

That's it. Your threads, spaces, and profile are now in `./deplexity-export/`.

---

## Usage

### Authentication

```bash
# Interactive browser login (recommended)
deplexity login

# Headless/server: provide session cookie directly
deplexity login --cookie "YOUR_SESSION_TOKEN"

# Check session status
deplexity status

# Remove saved session
deplexity logout
```

The session token is stored at `~/.config/deplexity/session.json` (mode 0600).

### Export

```bash
# Export with defaults (JSON + Markdown)
deplexity export

# Export to a specific directory
deplexity export -o ~/backups/perplexity

# Export as PDF
deplexity export -f pdf

# Export all formats
deplexity export -f json -f markdown -f pdf

# Re-fetch thread index even if a cached one exists
deplexity export --refresh

# Verbose output (show API calls and timing)
deplexity export -v

# Export only threads (skip spaces/profile)
deplexity export --no-spaces --no-profile

# Adjust rate limiting (milliseconds between requests)
deplexity export --delay 1000

# Control PDF parallelism (default: auto-detect CPU count)
deplexity export -f pdf --pdf-workers 4
```

Files you uploaded with your questions (images, documents) are not exported by
default. Add `--attachments` to download them:

```bash
deplexity export --attachments
```

They go into `threads/<thread-id>/attachments/`. Each file is named
`<8-hex-id>-<original-name>`, where the id keeps equal names from overwriting each
other. Every entry in `thread.json` lists its attachments (`name`, `file`, `size`),
and `thread.md` links them under an **Attachments** heading. Space folders get their
own copy of each thread's attachments. Attachments are not included in PDFs.

Perplexity serves uploads through pre-signed links that expire after about ten
minutes, so a file can only be saved while its thread is being fetched. The first
run with `--attachments` therefore re-fetches every thread that was cached without
them, once. After that, threads are skipped again unless a recorded file is
missing. A file that fails to download does not stop the export: it is listed in
`manifest.json` under `attachments.failed` and retried on the next run. A single
file larger than 100 MB is treated as a failure.

### Resumable Exports

Export runs in two phases:

1. **Phase 1 — Index**: Fetches the list of all threads and caches it in `thread_index.json`. If listing is interrupted, the next run re-lists from the beginning and uses the incomplete cache only to preserve per-thread retry metadata. The upstream list is newest-first and can reorder, so a saved numeric offset is not a safe resume point.
2. **Phase 2 — Details**: Fetches full content for each thread. Already-fetched threads are skipped automatically.

Use `--refresh` to force re-fetching the thread index (e.g., after new conversations).

If one or more thread details cannot be fetched or written after retries, Deplexity
keeps and renders every thread that completed successfully, writes the failure
details to `manifest.json`, and exits with a non-zero status. Re-run the same export
command to retry the missing threads from the saved checkpoints. A cancelled export
stops immediately instead of being reported as a partial success.

### Output Structure

```
deplexity-export/
├── manifest.json              # Export metadata, thread completeness, and failures
├── thread_index.json          # Cached thread list (for resumable exports)
├── profile/
│   └── user.json
├── account/                   # Account-wide data (not tied to any space)
│   ├── account.json           # Global skills metadata (referenced bodies below)
│   ├── global-skills.md       # Global skills overview (Markdown)
│   └── skills/                # Global skills' SKILL.md bodies
│       └── <skill-name>.md
├── spaces/
│   ├── index.json
│   ├── spaces.md
│   └── <space-name>-<space-id>/
│       ├── space.json         # Incl. AI instructions, suggested queries, primers, skills metadata
│       ├── skills/            # Attached skills' SKILL.md bodies (referenced by space.json)
│       │   └── <skill-name>.md
│       └── threads/           # Self-contained copies of this space's threads
│           └── <thread-slug>/
│               ├── thread.json
│               ├── thread.md
│               ├── thread.pdf
│               └── sources.json
└── threads/                   # Canonical flat list of all threads
    └── <thread-slug>/
        ├── thread.json
        ├── thread.md
        ├── thread.pdf
        ├── sources.json
        └── attachments/             # Only with --attachments
            └── <id>-<original-name>
```

Each space folder is self-contained — you can ZIP and share a single space without needing the top-level `threads/` directory.

Space exports capture each space's full context: its custom AI instructions, description, suggested queries, primers, and any attached skills. Skill definitions are written as `SKILL.md` files under `spaces/<space-name>-<space-id>/skills/` and referenced from `space.json`. The stable ID suffix prevents different spaces whose names normalize to the same filesystem name from overwriting each other. Older name-only space directories are left untouched when exporting into an existing output directory.

Account-wide **global skills** apply to every request regardless of space, so they are exported once under `account/` rather than duplicated per space. They are captured when spaces are exported (skip them with `--no-spaces`) and are written to JSON and Markdown outputs (`account/account.json`, `account/global-skills.md`, and bodies under `account/skills/`); PDF-only exports do not include them.

For automation, `manifest.json` reports `threads_complete`, `expected_threads`,
and `failed_threads`. Each failure includes its thread UUID, optional title, stage
(`fetch`, `write`, or `load`), and error message. `counts.threads` remains the
number of threads present in the current export.

---

## Export Formats

| Format   | Best For                                    |
| -------- | ------------------------------------------- |
| JSON     | Programmatic access, backups, data pipelines |
| Markdown | Reading, note-taking apps, version control   |
| PDF      | Sharing, printing, archival                  |

---

## How It Works

1. **Login** — Opens a real Chrome window (or uses a provided cookie). Captures the `__Secure-next-auth.session-token` cookie after you authenticate normally.
2. **Fetch** — Uses Perplexity's internal REST API with your session cookie. TLS fingerprinting matches a real Chrome browser via [utls](https://github.com/refraction-networking/utls). Adaptive rate limiting keeps requests under Perplexity's anti-abuse threshold (~2 req/s).
3. **Export** — Converts raw API responses into clean domain models, then renders them in your chosen format(s). Exports are resumable — interrupted runs pick up where they left off.

No credentials are stored — only the session cookie (which expires in ~7 days).

---

## Building

Requires [Bazel 9.1+](https://bazel.build/install):

```bash
# Build
bazel build //cmd/deplexity

# Build with version stamping
bazel build //cmd/deplexity --config=release

# Run tests
bazel test //...

# Regenerate BUILD files after code changes
bazel run //:gazelle
```

Or with plain Go:

```bash
go build -o deplexity ./cmd/deplexity
go test ./...
```

---

## Architecture

```
cmd/deplexity/       CLI entrypoint (Kong framework)
internal/
├── api/             Raw API types + data fetching
├── auth/            Browser login, cookie management, session validation
├── client/          HTTP client: utls transport, adaptive rate limiting, retry with backoff
├── export/          JSON, Markdown, and PDF renderers
└── models/          Clean domain models (decoupled from API shape)
```

Key design decisions:
- **Pure Go** — Single static binary, no CGO, no external tools
- **Context propagation** — All operations are cancellable (first Ctrl+C cancels gracefully, second force-quits)
- **Layered architecture** — API types are separate from domain models; exporters are independent
- **Cloudflare bypass** — Chrome TLS fingerprint via `refraction-networking/utls`
- **Adaptive rate limiting** — Delay doubles after 429s, halves after sustained success
- **Two-phase export** — Thread index cached separately; detail fetching skips completed threads
- **PDF via gpdf** — Declarative grid layout, no browser/wkhtmltopdf/LaTeX dependency

---

## Requirements

- **Go 1.26+** (build only)
- **Chrome/Chromium** (for `deplexity login` only — auto-downloaded if missing)
- No runtime dependencies for `export` or `login --cookie`

---

## Security

- Session tokens are stored with `0600` permissions in `~/.config/deplexity/`
- No credentials are ever logged or transmitted to third parties
- Requests to Perplexity use the same TLS fingerprint and headers as a real Chrome session
- Skill bodies and attachments are downloaded from the pre-signed storage URLs (Amazon S3) that Perplexity returns; these fetches deliberately carry **no** session cookie or Perplexity headers, so your credentials are never sent off-platform. The pre-signed links themselves are never written to the export or to error messages

---

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch
3. Run `bazel test //...` (all tests must pass)
4. Run `bazel run //:gazelle` if you added/removed files
5. Submit a pull request

---

## License

[MIT](LICENSE)

---

## Disclaimer

Deplexity is an independent, community-built tool. It is not affiliated with, endorsed by, or sponsored by Perplexity AI. Use responsibly and in accordance with Perplexity's terms of service.
