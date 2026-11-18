package stats

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"
)

// Row is one feature's use in the files kept.
type Row struct {
	Feature Feature `json:"feature"`
	Context string  `json:"context"`
	What    string  `json:"what"`
	Last7   int     `json:"last_7d"`
	Last30  int     `json:"last_30d"`
	Total   int     `json:"total"`
	Keys    int     `json:"by_key,omitempty"`
	Mouse   int     `json:"by_mouse,omitempty"`
	First   string  `json:"first,omitempty"`
	Last    string  `json:"last,omitempty"`
}

// Report is what ccs stats prints.
type Report struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir"`
	Since   string `json:"since,omitempty"` // the oldest event kept
	Used    []Row  `json:"used"`
	Never   []Row  `json:"never_used"`
}

// Build reads every month file and counts each registered feature.
func Build(at time.Time) Report {
	dir := Dir()
	r := Report{Enabled: Enabled(), Dir: dir}
	rows := make(map[Feature]*Row, len(Registry))
	firsts, lasts := map[Feature]int64{}, map[Feature]int64{}
	for _, i := range Registry {
		rows[i.ID] = &Row{Feature: i.ID, Context: i.ID.Context(), What: i.What}
	}
	var oldest int64
	for _, name := range months(dir) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e Event
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			row := rows[e.F]
			if row == nil {
				continue // a feature since dropped, or a line that isn't ours
			}
			if oldest == 0 || e.T < oldest {
				oldest = e.T
			}
			age := at.Sub(time.Unix(e.T, 0))
			row.Total++
			if age <= 7*24*time.Hour {
				row.Last7++
			}
			if age <= 30*24*time.Hour {
				row.Last30++
			}
			switch e.Via {
			case Key:
				row.Keys++
			case Mouse:
				row.Mouse++
			}
			if firsts[e.F] == 0 || e.T < firsts[e.F] {
				firsts[e.F] = e.T
			}
			if e.T > lasts[e.F] {
				lasts[e.F] = e.T
			}
		}
		f.Close()
	}
	if oldest > 0 {
		r.Since = day(oldest)
	}
	order := make(map[Feature]int, len(Registry))
	for n, i := range Registry {
		order[i.ID] = n
		row := rows[i.ID]
		if row.Total == 0 {
			r.Never = append(r.Never, *row)
			continue
		}
		row.First, row.Last = day(firsts[i.ID]), day(lasts[i.ID])
		r.Used = append(r.Used, *row)
	}
	ctxOrder := map[string]int{"tui": 0, "cli": 1, "mcp": 2, "watch": 3}
	sort.SliceStable(r.Used, func(a, b int) bool {
		x, y := r.Used[a], r.Used[b]
		if x.Context != y.Context {
			return ctxOrder[x.Context] < ctxOrder[y.Context]
		}
		if x.Last30 != y.Last30 {
			return x.Last30 > y.Last30
		}
		if x.Total != y.Total {
			return x.Total > y.Total
		}
		return order[x.Feature] < order[y.Feature]
	})
	return r
}

func day(t int64) string { return time.Unix(t, 0).Format("2006-01-02") }

// Run is ccs stats.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	off := fs.Bool("off", false, "stop recording feature use")
	on := fs.Bool("on", false, "record feature use again")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: ccs stats [--json] [--off | --on]

How often each ccs feature was used on this machine, over 7 and 30 days,
with first and last use, then the features never used.

ccs records only a feature id, the time, the ccs version, where it ran (tui,
cli, mcp, watch) and, in the list, key or mouse: never session names, prompts,
paths, repos, PRs or message text. Files: %s, one a month, %d months kept.
Nothing is ever sent anywhere. Off: ccs stats --off, or CCS_NO_STATS=1.

`, Dir(), Keep)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *off && *on {
		fmt.Fprintln(stderr, "ccs stats: --off or --on, not both")
		return 2
	}
	if *off || *on {
		if err := SetEnabled(*on); err != nil {
			fmt.Fprintln(stderr, "ccs stats:", err)
			return 1
		}
		if *on {
			fmt.Fprintln(stdout, "Feature stats on")
		} else {
			fmt.Fprintln(stdout, "Feature stats off: nothing more is recorded (the files kept are left as they are)")
		}
		return 0
	}
	r := Build(time.Now())
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return 0
	}
	state := "on"
	if !r.Enabled {
		state = "off (ccs stats --on)"
	}
	fmt.Fprintf(stdout, "Feature stats: %s · %s\n", state, r.Dir)
	if r.Since == "" {
		fmt.Fprintln(stdout, "Nothing recorded yet.")
		return 0
	}
	fmt.Fprintf(stdout, "Since %s\n\n", r.Since)
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FEATURE\t7D\t30D\tALL\tKEY/MOUSE\tFIRST\tLAST\tWHAT")
	for _, row := range r.Used {
		km := ""
		if row.Context == "tui" && row.Keys+row.Mouse > 0 {
			km = fmt.Sprintf("%d/%d", row.Keys, row.Mouse)
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", row.Feature, row.Last7, row.Last30, row.Total, km, row.First, row.Last, row.What)
	}
	w.Flush()
	if len(r.Never) > 0 {
		fmt.Fprintf(stdout, "\nNever used since %s:\n", r.Since)
		w = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, row := range r.Never {
			fmt.Fprintf(w, "  %s\t%s\n", row.Feature, row.What)
		}
		w.Flush()
	}
	return 0
}
