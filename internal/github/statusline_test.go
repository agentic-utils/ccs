package github

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStatusLineFiles(t *testing.T) {
	live := claimEnv(t)
	dir := t.TempDir()
	old := StatusLineDir
	StatusLineDir = func() string { return dir }
	t.Cleanup(func() { StatusLineDir = old })
	now := time.Now()
	zero := 0
	writeWatchState(t, now.Add(-time.Minute),
		&WatchedPR{URL: claimA, State: "open", Checks: "success", Review: "approved", Threads: &zero},
		&WatchedPR{URL: claimB, State: "open", Checks: "failing", Failing: []string{"test"}},
		&WatchedPR{URL: claimC, State: "open", Checks: "pending"},
		&WatchedPR{URL: "https://github.com/o/r/pull/4", State: "open", Draft: true},
		&WatchedPR{URL: "https://github.com/o/r/pull/5", State: "open", Checks: "success", Review: "required"},
		&WatchedPR{URL: "https://github.com/o/r/pull/9", State: "merged"},
	)
	read := func(id string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, statusLinePrefix+id))
		return string(b), err == nil
	}
	for _, u := range []string{claimC, claimB, "https://github.com/o/r/pull/4", "https://github.com/o/r/pull/5", claimA, "https://github.com/o/r/pull/9"} {
		if _, err := Claim(u, false, now); err != nil {
			t.Fatal(err)
		}
	}
	want := "0 " + claimA + "\n1 https://github.com/o/r/pull/5\n2 " + claimC + "\n3 " + claimB + "\n4 https://github.com/o/r/pull/4\n"
	if got, _ := read("s-alpha"); got != want {
		t.Fatalf("ccs claim writes its session's file, by bucket, open PRs only:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, statusLineSentinel)); err == nil {
		t.Fatal("only ccs watch says it keeps the files")
	}

	os.WriteFile(filepath.Join(dir, statusLinePrefix+"gone"), []byte("1 x\n"), 0o600)
	os.WriteFile(filepath.Join(dir, statusLinePrefix+"s-alpha.tmp"), []byte("the script's"), 0o600)
	stamp := now.Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, statusLinePrefix+"s-alpha"), stamp, stamp)
	if err := WriteStatusLines("", now); err != nil {
		t.Fatal(err)
	}
	if _, ok := read("gone"); ok {
		t.Error("a session with no claimed open PR has no file")
	}
	if _, ok := read("s-alpha.tmp"); !ok {
		t.Error("the script's own temp file is left alone")
	}
	if fi, _ := os.Stat(filepath.Join(dir, statusLinePrefix+"s-alpha")); !fi.ModTime().Equal(stamp) {
		t.Error("an unchanged file isn't rewritten")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, statusLineSentinel)); string(b) != strconv.FormatInt(now.Unix(), 10)+"\n" {
		t.Errorf("the sentinel holds the pass's time: %q", b)
	}

	Claim(claimB, true, now)
	if got, _ := read("s-alpha"); got != "0 "+claimA+"\n1 https://github.com/o/r/pull/5\n2 "+claimC+"\n4 https://github.com/o/r/pull/4\n" {
		t.Errorf("a release rewrites it at once:\n%s", got)
	}
	_ = live
}

func TestStatusBucketBlocked(t *testing.T) {
	if b := statusBucket(&WatchedPR{Checks: "success", Review: "approved", BlockedBy: []string{"o/r#1"}}); b != 1 {
		t.Errorf("waiting for another PR is waiting: %d", b)
	}
	if b := statusBucket(&WatchedPR{Checks: "success", Review: "approved", BlockedBy: []string{"o/r#1"}, AutoMerge: true}); b != 3 {
		t.Errorf("auto-merge on a blocked PR needs you: %d", b)
	}
}
