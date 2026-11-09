package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A headless `codex exec` run records nothing about who started it, so its
// parent is caught while it runs: on each live tick, a running `codex exec`
// whose process descends from a live claude process belongs to that
// claude's session. The link is kept on disk (execParentsPath), so the run
// stays nested after it exits and after ccs restarts. Runs ccs never saw
// alive keep the prompt match (findSpawnParent).

// psTree is every process's pid, parent pid and command line. Stubbed in tests.
var psTree = func() (string, error) {
	out, err := runBounded(3*time.Second, nil, "ps", "-axww", "-o", "pid=,ppid=,command=")
	return string(out), err
}

func execParentsPath() string {
	return filepath.Join(filepath.Dir(marksPath), "codex-exec-parents.json")
}

// execParents maps a codex exec thread to the Claude session that ran it.
// seen remembers each exec process already linked (by pid and start), so a
// long run isn't looked up again every tick.
var execParents = struct {
	sync.Mutex
	loaded bool
	m      map[string]string
	seen   map[string]bool
}{}

func loadExecParents() {
	if execParents.loaded {
		return
	}
	execParents.loaded, execParents.m, execParents.seen = true, map[string]string{}, map[string]bool{}
	if data, err := os.ReadFile(execParentsPath()); err == nil {
		json.Unmarshal(data, &execParents.m)
	}
}

// execParentOf is the Claude session that ran codex thread, "" if none was seen.
func execParentOf(thread string) string {
	execParents.Lock()
	defer execParents.Unlock()
	loadExecParents()
	return execParents.m[thread]
}

// codexValueFlags take a value, so the word after one isn't the subcommand.
var codexValueFlags = map[string]bool{
	"-c": true, "--config": true, "--enable": true, "--disable": true, "-p": true, "--profile": true,
	"-m": true, "--model": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true,
	"-C": true, "--cd": true, "--add-dir": true, "-i": true, "--image": true, "--remote": true,
	"--remote-auth-token-env": true, "--local-provider": true,
}

// codexExecArgs: a command line that is `codex [options] exec ...`, and the
// folder it works in when -C/--cd says ("" otherwise).
func codexExecArgs(fields []string) (bool, string) {
	for i, f := range fields {
		if b := filepath.Base(f); b != "codex" && b != "codex.js" {
			continue
		}
		cd := ""
		for j := i + 1; j < len(fields); j++ {
			a := fields[j]
			if codexValueFlags[a] && j+1 < len(fields) {
				if a == "-C" || a == "--cd" {
					cd = fields[j+1]
				}
				j++
				continue
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			return a == "exec" || a == "e", cd
		}
		return false, ""
	}
	return false, ""
}

// noteCodexExecs links the running codex exec runs to the live Claude
// session whose process they descend from. sessions is the live tick's scan.
func noteCodexExecs(sessions map[string]liveSession) {
	claude := map[int]string{}
	for id, s := range sessions {
		if !s.codex {
			claude[s.pid] = id
		}
	}
	if len(claude) == 0 {
		return
	}
	out, err := psTree()
	if err != nil {
		return
	}
	parent := map[int]int{}
	type run struct {
		pid int
		cd  string
	}
	var runs []run
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		parent[pid] = ppid
		if ok, cd := codexExecArgs(f[2:]); ok {
			runs = append(runs, run{pid, cd})
		}
	}
	if len(runs) == 0 {
		return
	}
	pids := map[string]int{}
	for _, r := range runs {
		pids[strconv.Itoa(r.pid)] = r.pid
	}
	starts := processStartTimes(pids)

	execParents.Lock()
	defer execParents.Unlock()
	loadExecParents()
	changed := false
	var rollouts []string
	for _, r := range runs {
		start := starts[r.pid]
		if start.IsZero() {
			continue // a recycled pid can't be ruled out
		}
		key := strconv.Itoa(r.pid) + " " + start.UTC().Format(time.RFC3339)
		if execParents.seen[key] {
			continue
		}
		session := ""
		for p, n := parent[r.pid], 0; p > 1 && n < 30; p, n = parent[p], n+1 {
			if id, ok := claude[p]; ok {
				session = id
				break
			}
		}
		if session == "" {
			execParents.seen[key] = true // not started by a Claude session
			continue
		}
		cwd := r.cd
		if cwd == "" {
			cwd = codexProcCwd(r.pid)
		} else if !filepath.IsAbs(cwd) {
			if base := codexProcCwd(r.pid); base != "" {
				cwd = filepath.Join(base, cwd)
			}
		}
		if cwd == "" {
			continue
		}
		if rollouts == nil {
			rollouts = codexRollouts()
		}
		thread := execThread(rollouts, cwd, start)
		if thread == "" {
			continue // no rollout yet: tried again next tick
		}
		execParents.seen[key] = true
		if execParents.m[thread] != session {
			execParents.m[thread] = session
			changed = true
		}
	}
	if changed {
		saveExecParents()
	}
}

// execThread is the exec run's thread: the newest exec rollout in cwd that
// began at or after the process did.
func execThread(rollouts []string, cwd string, start time.Time) string {
	best, bestAt := "", time.Time{}
	for _, p := range rollouts {
		info, err := os.Stat(p)
		if err != nil || info.ModTime().Before(start) {
			continue
		}
		m, ok := readCodexMeta(p)
		if !ok || m.Cwd != cwd || codexSourceName(m.Source) != "exec" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m.Timestamp)
		if err != nil || at.Before(start.Add(-2*time.Second)) || at.Before(bestAt) {
			continue
		}
		best, bestAt = m.ID, at
	}
	return best
}

// codexSourceName is a session_meta source when it's a plain string ("exec",
// "cli"...), "" for a subagent's object.
func codexSourceName(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

func saveExecParents() {
	data, err := json.Marshal(execParents.m)
	if err != nil {
		return
	}
	path := execParentsPath()
	os.MkdirAll(filepath.Dir(path), 0o755)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-exec-parents-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
	}
}
