package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================================
// Prune - shrink conversation files by removing duplicate / redundant data
// ============================================================================

// pruneOpts selects which categories of redundant data to remove. Conversation
// (user/assistant) messages are never touched.
type pruneOpts struct {
	dropSnapshots    bool // drop file-history-snapshot lines (rewind/checkpoint backups)
	stripToolResults bool // remove the toolUseResult field (a copy of the tool_result already in message.content)
}

type pruneStats struct {
	bytesIn          int64
	bytesOut         int64
	droppedSnapshots int
	strippedResults  int
	convLinesIn      int // user/assistant lines seen
	convLinesOut     int // ... and kept (invariant: must equal convLinesIn)
}

// pruneLine applies the transforms to one JSONL line. It returns the output
// bytes (nil = drop the line), the line's "type", and whether it was
// dropped/stripped. Unparseable lines pass through verbatim.
func pruneLine(line []byte, opts pruneOpts) (out []byte, typ string, dropped, stripped bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		return line, "", false, false
	}
	if raw, ok := obj["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	if opts.dropSnapshots && typ == "file-history-snapshot" {
		return nil, typ, true, false
	}
	if opts.stripToolResults {
		if _, ok := obj["toolUseResult"]; ok {
			delete(obj, "toolUseResult")
			b, err := json.Marshal(obj)
			if err != nil {
				return line, typ, false, false // keep original on marshal error
			}
			return b, typ, false, true
		}
	}
	return line, typ, false, false
}

// pruneStream reads JSONL from r and writes the pruned version to w (w may be
// nil to only measure). It never drops or modifies user/assistant lines.
func pruneStream(r io.Reader, w io.Writer, opts pruneOpts) (pruneStats, error) {
	var st pruneStats
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		st.bytesIn += int64(len(line)) + 1
		out, typ, dropped, stripped := pruneLine(line, opts)
		isConv := typ == "user" || typ == "assistant"
		if isConv {
			st.convLinesIn++
		}
		if dropped {
			st.droppedSnapshots++
			continue
		}
		if stripped {
			st.strippedResults++
		}
		if isConv {
			st.convLinesOut++
		}
		if w != nil {
			if _, err := w.Write(out); err != nil {
				return st, err
			}
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return st, err
			}
		}
		st.bytesOut += int64(len(out)) + 1
	}
	return st, scanner.Err()
}

// pruneFile prunes one conversation file. With write=true it streams to
// <path>.pruned and atomically replaces path, aborting (no replace) if the
// conversation line count would change. With write=false it only measures.
func pruneFile(path string, write bool, opts pruneOpts) (pruneStats, error) {
	in, err := os.Open(path)
	if err != nil {
		return pruneStats{}, err
	}
	defer in.Close()

	if !write {
		return pruneStream(in, nil, opts)
	}

	tmpPath := path + ".pruned"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return pruneStats{}, err
	}
	bw := bufio.NewWriter(tmp)
	st, err := pruneStream(in, bw, opts)
	if err == nil {
		err = bw.Flush()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && st.convLinesIn != st.convLinesOut {
		err = fmt.Errorf("integrity check failed: %d conversation lines in, %d out", st.convLinesIn, st.convLinesOut)
	}
	if err != nil {
		os.Remove(tmpPath)
		return st, err
	}
	return st, os.Rename(tmpPath, path)
}

// findPrunableFiles returns .jsonl files at or above minSize, largest first.
func findPrunableFiles(minSize int64) ([]string, error) {
	type fileSize struct {
		path string
		size int64
	}
	var found []fileSize
	err := filepath.Walk(getProjectsDir(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".jsonl") && info.Size() >= minSize {
			found = append(found, fileSize{path, info.Size()})
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool { return found[i].size > found[j].size })
	paths := make([]string, len(found))
	for i, f := range found {
		paths[i] = f.path
	}
	return paths, err
}

// shortPath shows the project dir + filename for readable reporting.
func shortPath(p string) string {
	return filepath.Join(filepath.Base(filepath.Dir(p)), filepath.Base(p))
}

func runPrune(args []string) {
	apply, yes := false, false // dry run by default; --apply to actually rewrite
	minSizeMB := int64(50)
	opts := pruneOpts{dropSnapshots: true, stripToolResults: true}
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			printPruneHelp()
			return
		case a == "--apply":
			apply = true
		case a == "--dry-run":
			apply = false // explicit; this is already the default
		case a == "-y" || a == "--yes":
			yes = true
		case a == "--no-snapshots":
			opts.dropSnapshots = false
		case a == "--no-tool-results":
			opts.stripToolResults = false
		case strings.HasPrefix(a, "--min-size="):
			fmt.Sscanf(strings.TrimPrefix(a, "--min-size="), "%d", &minSizeMB)
		default:
			fmt.Fprintf(os.Stderr, "unknown prune flag: %s (try ccs prune --help)\n", a)
			os.Exit(2)
		}
	}
	if !opts.dropSnapshots && !opts.stripToolResults {
		fmt.Fprintln(os.Stderr, "nothing to prune: both categories disabled")
		os.Exit(2)
	}

	files, err := findPrunableFiles(minSizeMB * 1024 * 1024)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error scanning conversations: %v\n", err)
		os.Exit(1)
	}
	if len(files) == 0 {
		fmt.Printf("No conversations >= %dMB to prune.\n", minSizeMB)
		return
	}

	report := func(st pruneStats, path string) {
		saved := st.bytesIn - st.bytesOut
		fmt.Printf("  %-46s %8s -> %8s  (-%s)\n", shortPath(path), formatBytes(st.bytesIn), formatBytes(st.bytesOut), formatBytes(saved))
	}

	if !apply {
		fmt.Printf("Dry run (no changes). Conversations >= %dMB:\n\n", minSizeMB)
		var in, out int64
		for _, f := range files {
			st, err := pruneFile(f, false, opts)
			if err != nil {
				fmt.Printf("  %-46s error: %v\n", shortPath(f), err)
				continue
			}
			report(st, f)
			in += st.bytesIn
			out += st.bytesOut
		}
		fmt.Printf("\nWould reclaim %s across %d files. Re-run with --apply to prune.\n", formatBytes(in-out), len(files))
		return
	}

	if !yes {
		var total int64
		for _, f := range files {
			if info, e := os.Stat(f); e == nil {
				total += info.Size()
			}
		}
		fmt.Printf("Prune %d conversations (%s)? Rewrites them in place, removing duplicate tool\nresults and snapshot backups - dialogue is preserved. [y/N] ", len(files), formatBytes(total))
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			fmt.Println("Aborted.")
			return
		}
	}

	var in, out int64
	for _, f := range files {
		st, err := pruneFile(f, true, opts)
		if err != nil {
			fmt.Printf("  %-46s FAILED: %v\n", shortPath(f), err)
			continue
		}
		report(st, f)
		in += st.bytesIn
		out += st.bytesOut
	}
	fmt.Printf("\nReclaimed %s across %d files.\n", formatBytes(in-out), len(files))
}

func printPruneHelp() {
	fmt.Print(`ccs prune - shrink conversation files by removing redundant data

Removes data that duplicates content kept elsewhere, so pruned conversations
still resume with full dialogue:
  - toolUseResult fields (a copy of the tool_result already in message.content)
  - file-history-snapshot lines (rewind/checkpoint backups; pruning loses
    rewind history, not the conversation)

User and assistant messages are never modified. Each file is rewritten only if
its conversation line count is unchanged.

By default this is a dry run that only previews savings - pass --apply to
actually rewrite the files.

Usage: ccs prune [flags]

Flags:
  --apply              Actually rewrite files (default is a dry-run preview)
  --min-size=N         Only consider files >= N MB (default: 50)
  --no-tool-results    Keep toolUseResult fields
  --no-snapshots       Keep file-history-snapshot lines
  -y, --yes            Skip the confirmation prompt (with --apply)
  -h, --help           Show this help

Examples:
  ccs prune                        Preview savings across files >= 50MB
  ccs prune --apply                Prune files >= 50MB (after confirmation)
  ccs prune --apply --min-size=200 Prune files >= 200MB
  ccs prune --apply --no-tool-results -y   Only drop snapshot backups, no prompt
`)
}
