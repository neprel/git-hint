package library

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Budget is the finite allowance of one job. It is fixed when the job starts.
type Budget struct {
	Requests int `json:"requests"`
	Retries  int `json:"retries"`
	Seconds  int `json:"seconds"`
	Trials   int `json:"trials,omitempty"`
}

// Entry is one journal line.
type Entry struct {
	At       string  `json:"at"`
	Job      string  `json:"job"`
	Type     string  `json:"type"` // start, consume, end
	Kind     string  `json:"kind,omitempty"`
	Budget   *Budget `json:"budget,omitempty"`
	Requests int     `json:"requests,omitempty"`
	Retries  int     `json:"retries,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	Trials   int     `json:"trials,omitempty"`
	Status   string  `json:"status,omitempty"`
}

// JobState is a job as the journal records it.
type JobState struct {
	Job      string
	Kind     string
	Budget   Budget
	Requests int
	Retries  int
	Seconds  float64
	Trials   int
	Ended    bool
	Status   string
	Damaged  bool // a journal line could not be read: nothing more may be consumed
}

// Remaining reports whether the job may consume more, and the reason when not.
func (j JobState) Remaining() (ok bool, reason string) {
	switch {
	case j.Damaged:
		return false, "journal_damaged"
	case j.Ended:
		return false, "job_ended"
	case j.Requests >= j.Budget.Requests && j.Budget.Requests > 0:
		return false, "request_limit"
	case j.Retries > j.Budget.Retries:
		return false, "retry_limit"
	case j.Budget.Seconds > 0 && j.Seconds >= float64(j.Budget.Seconds):
		return false, "time_limit"
	case j.Budget.Trials > 0 && j.Trials >= j.Budget.Trials:
		return false, "trial_limit"
	}
	return true, ""
}

var jobID = regexp.MustCompile(`^jb-[0-9a-f]{16}$`)

// ErrJobExists is returned when a job ID is started twice: a restart must resume the
// recorded budget, never set a new one.
var ErrJobExists = errors.New("job already started; resume it with its recorded budget")

func (l *Library) appendEntry(e Entry) error {
	if !jobID.MatchString(e.Job) {
		return fmt.Errorf("invalid job ID %q", e.Job)
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	p := l.path("journal.jsonl")
	if err := l.checkTarget(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return err
	}
	defer unlockFile(f)
	if e.Type == "start" {
		jobs, err := l.readJobs(f)
		if err != nil {
			return err
		}
		if _, ok := jobs[e.Job]; ok {
			return fmt.Errorf("%w: %s", ErrJobExists, e.Job)
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// StartJob records a new job and its budget.
func (l *Library) StartJob(job, kind string, b Budget) error {
	if b.Seconds <= 0 || (b.Requests <= 0 && b.Trials <= 0) {
		return errors.New("a job needs a finite time limit and a request or trial limit")
	}
	return l.appendEntry(Entry{Job: job, Type: "start", Kind: kind, Budget: &b})
}

// Consume records resources that a job used. It refuses when the job has no budget left;
// the use is recorded first, so a crash after the work never loses it.
func (l *Library) Consume(job string, requests, retries, trials int, seconds float64) error {
	st, err := l.Job(job)
	if err != nil {
		return err
	}
	if ok, reason := st.Remaining(); !ok {
		return fmt.Errorf("job %s cannot consume more: %s", job, reason)
	}
	return l.appendEntry(Entry{Job: job, Type: "consume", Requests: requests, Retries: retries, Trials: trials, Seconds: seconds})
}

// EndJob records the end of a job.
func (l *Library) EndJob(job, status string) error {
	return l.appendEntry(Entry{Job: job, Type: "end", Status: status})
}

// Job replays the journal for one job.
func (l *Library) Job(job string) (JobState, error) {
	jobs, err := l.Jobs()
	if err != nil {
		return JobState{}, err
	}
	st, ok := jobs[job]
	if !ok {
		return JobState{}, fmt.Errorf("unknown job %s", job)
	}
	return st, nil
}

// Jobs replays the whole journal.
func (l *Library) Jobs() (map[string]JobState, error) {
	f, err := os.Open(l.path("journal.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]JobState{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return l.readJobs(f)
}

// readJobs replays lines. A line that cannot be read (for example a torn last line after
// a crash) marks every job that is not ended as damaged: its consumption is unknown, so it
// may not continue.
func (l *Library) readJobs(f *os.File) (map[string]JobState, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	jobs := map[string]JobState{}
	damaged := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil || !jobID.MatchString(e.Job) {
			damaged = true
			continue
		}
		st := jobs[e.Job]
		switch e.Type {
		case "start":
			if st.Job == "" && e.Budget != nil {
				st = JobState{Job: e.Job, Kind: e.Kind, Budget: *e.Budget}
			}
		case "consume":
			st.Requests += e.Requests
			st.Retries += e.Retries
			st.Trials += e.Trials
			st.Seconds += e.Seconds
		case "end":
			st.Ended, st.Status = true, e.Status
		default:
			damaged = true
		}
		if st.Job == "" { // a line for a job without a start line
			damaged = true
			continue
		}
		jobs[e.Job] = st
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if damaged {
		for k, st := range jobs {
			if !st.Ended {
				st.Damaged = true
				jobs[k] = st
			}
		}
	}
	return jobs, nil
}

// ---------------------------------------------------------------------------
// pins, inventory, retention and deletion

// Pins returns the pinned record names (eval versions or suites).
func (l *Library) Pins() (map[string]bool, error) {
	out := map[string]bool{}
	b, err := os.ReadFile(l.path("pins.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, err
	}
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// Pin marks or unmarks records that retention must keep.
func (l *Library) Pin(name string, on bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	pins, err := l.Pins()
	if err != nil {
		return err
	}
	if on {
		pins[name] = true
	} else {
		delete(pins, name)
	}
	names := make([]string, 0, len(pins))
	for n := range pins {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(names)
	return l.writeNew(l.path("pins.json"), append(b, '\n'), false)
}

// External is a runner artifact outside the library (a container image, a volume, a job
// directory). The library records it so that deletion can report what remains.
type External struct {
	Kind    string `json:"kind"` // image, volume, job_dir
	Name    string `json:"name"`
	Removed bool   `json:"removed,omitempty"`
}

// RecordExternal adds (or, with Removed, closes) an inventory entry.
func (l *Library) RecordExternal(e External) error {
	line, _ := json.Marshal(e)
	p := l.path("external.jsonl")
	if err := l.checkTarget(p); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// Inventory lists the runner artifacts that are recorded and not removed.
func (l *Library) Inventory() ([]External, error) {
	b, err := os.ReadFile(l.path("external.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	live := map[External]bool{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e External
		if json.Unmarshal(line, &e) != nil || e.Name == "" {
			continue
		}
		removed := e.Removed
		e.Removed = false
		live[e] = !removed
	}
	var out []External
	for e, ok := range live {
		if ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].Name < out[j].Kind+out[j].Name })
	return out, nil
}

var digestRe = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// created returns the creation time of a record.
func created(m map[string]any) time.Time {
	for _, k := range []string{"created_at", "started_at"} {
		if s, ok := m[k].(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

var kinds = []string{"episode", "eval", "suite", "trial"}

// pinnedClosure returns the pinned names and the eval versions of pinned suites.
func (l *Library) pinnedClosure() (map[string]bool, error) {
	pins, err := l.Pins()
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for n := range pins {
		keep[n] = true
		if strings.HasPrefix(n, "su-") {
			var s struct {
				Members []struct {
					Eval string `json:"eval"`
				} `json:"members"`
			}
			if l.Manifest("suite", n, &s) == nil {
				for _, m := range s.Members {
					keep[m.Eval] = true
				}
			}
		}
	}
	return keep, nil
}

// Report says what a deletion removed and what remains outside the library.
type Report struct {
	Manifests int        `json:"manifests"`
	Objects   int        `json:"objects"`
	Kept      []string   `json:"kept_pinned,omitempty"`
	External  []External `json:"external_remaining,omitempty"`
}

// Prune deletes unpinned records created before now - retention and then every object
// that no remaining record references.
func (l *Library) Prune(now time.Time, retentionDays int) (Report, error) {
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	return l.remove(func(kind, name string, m map[string]any) bool {
		t := created(m)
		return !t.IsZero() && t.Before(cutoff)
	})
}

// Forget deletes the named records (an eval ID removes all its versions). Pinned records
// are kept and reported.
func (l *Library) Forget(names []string) (Report, error) {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	return l.remove(func(kind, name string, _ map[string]any) bool {
		base, _, _ := strings.Cut(name, "@")
		return want[name] || want[base]
	})
}

func (l *Library) remove(match func(kind, name string, m map[string]any) bool) (Report, error) {
	lock, e := l.destructiveStateLock()
	if e != nil {
		return Report{}, e
	}
	defer lock.Unlock()
	if err := l.definitionRetentionProblem(); err != nil {
		return Report{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var rep Report
	keep, err := l.pinnedClosure()
	if err != nil {
		return rep, err
	}
	for _, k := range kinds {
		names, err := l.Manifests(k)
		if err != nil {
			return rep, err
		}
		for _, n := range names {
			var m map[string]any
			if err := l.Manifest(k, n, &m); err != nil {
				continue
			}
			if !match(k, n, m) {
				continue
			}
			base, _, _ := strings.Cut(n, "@")
			if keep[n] || keep[base] {
				rep.Kept = append(rep.Kept, n)
				continue
			}
			p := l.path("manifests", k, n+".json")
			if err := l.checkTarget(p); err != nil {
				return rep, err
			}
			if err := os.Remove(p); err == nil {
				rep.Manifests++
			}
		}
	}
	n, err := l.collectGarbage()
	rep.Objects = n
	if err != nil {
		return rep, err
	}
	rep.External, err = l.Inventory()
	return rep, err
}

// collectGarbage removes objects that no manifest references.
func (l *Library) collectGarbage() (int, error) {
	if err := l.definitionRetentionProblem(); err != nil {
		return 0, err
	}
	live := map[string]bool{}
	for _, k := range kinds {
		names, err := l.Manifests(k)
		if err != nil {
			return 0, err
		}
		for _, n := range names {
			b, err := os.ReadFile(l.path("manifests", k, n+".json"))
			if err != nil {
				continue
			}
			for _, d := range digestRe.FindAll(b, -1) {
				live[string(d)] = true
			}
		}
	}
	// index records (imported sessions, snapshot maps, bookkeeping) keep their objects too
	filepath.WalkDir(l.path("index"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil {
			for _, x := range digestRe.FindAll(b, -1) {
				live[string(x)] = true
			}
		}
		return nil
	})
	// objects that live objects name (session texts, tree files) stay as well
	for changed := true; changed; {
		changed = false
		for d := range live {
			b, err := l.GetObject(d)
			if err != nil || len(b) > 64<<20 {
				continue
			}
			for _, x := range digestRe.FindAll(b, -1) {
				if !live[string(x)] {
					live[string(x)] = true
					changed = true
				}
			}
		}
	}
	removed := 0
	root := l.path("objects")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		dg := "sha256:" + filepath.Base(filepath.Dir(p)) + d.Name()
		if !live[dg] {
			if err := os.Remove(p); err == nil {
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// Purge deletes the whole library of the project and reports the runner artifacts that
// it recorded and that still exist outside it (the caller removes or lists them).
func (l *Library) Purge() (Report, error) {
	lock, e := l.destructiveStateLock()
	if e != nil {
		return Report{}, e
	}
	defer lock.Unlock()
	if err := l.definitionRetentionProblem(); err != nil {
		return Report{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ext, err := l.Inventory()
	if err != nil {
		return Report{}, err
	}
	root := l.path()
	if err := l.checkTarget(filepath.Join(root, "x")); err != nil {
		return Report{}, err
	}
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return Report{}, err
	}
	return Report{Objects: n, External: ext}, nil
}
