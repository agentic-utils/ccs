# CCS - Claude Code Search

## Overview

CLI tool to search and resume Claude Code conversations using fzf.

## Development

### Build & Test

```bash
go build -o ccs .
go test -v -cover
```

### Install locally

```bash
cp ccs /usr/local/bin/ccs
```

## Release Process

1. Update version in `main.go`:
   ```go
   const version = "X.Y.Z"
   ```

2. Commit changes:
   ```bash
   git add -A && git commit -m "feat/fix: description"
   ```

3. Push and tag:
   ```bash
   git push
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

4. CI will run tests, then release via GoReleaser and update Homebrew tap

## Version Bumping

- **Patch** (0.0.X): Bug fixes, minor tweaks
- **Minor** (0.X.0): New features, backwards compatible
- **Major** (X.0.0): Breaking changes

## Architecture

- `main.go` - Single file containing all logic
- `main_test.go` - Unit tests
- `.goreleaser.yaml` - Release configuration
- `.github/workflows/test.yaml` - CI test workflow (reusable)
- `.github/workflows/release.yaml` - Release workflow (calls test.yaml)

## Key Functions

- `getConversations()` - Loads all conversations from `~/.claude/projects/`
- `buildSearchLines()` - Creates fzf-compatible search lines (one per conversation)
- `showPreview()` - Renders preview with matches highlighted
- `parseConversationFile()` - Parses JSONL conversation files

## Conventions

- Use conventional commits (feat:, fix:, docs:, etc.)
- Run tests before releasing
- Keep it simple - single file is fine for this project
