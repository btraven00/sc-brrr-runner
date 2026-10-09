// sc-brrr-runner: the scoring service for the sc-brrr challenge.
//
// It drains the queue in a checkout of sc-brrr: every entry in incoming/<account>/ (a merged,
// reviewed submission) is scored by the challenge repo's own score.py (podman runner, main's
// code), oldest first. Then:
//
//   - scored (jobs ok or failed): the result is committed to the results checkout, the scoreboard
//     rebuilt and committed, and the entry moved to submissions/ (the log of scored entries);
//   - not scored (rejected, or setup failed before any job ran): the entry is moved to failed/ with
//     <name>-<ver>.outcome.json (the reason and the end of the log). To retry, git mv it back.
//
// score.py's contract: the last "OUTCOME {json}" line of its output and its exit code. No OUTCOME
// line means the scorer itself broke: the entry stays in incoming/ and the runner stops.
//
// Missing checkouts are cloned from -repo-url / -results-url. Git authenticates with $GH_TOKEN when it
// is set, else with whatever git is configured with (e.g. `gh auth setup-git`). -listen serves the
// live log of the running entry and the recent outcomes (localhost by default: no auth).
//
//	sc-brrr-runner [-repo ../sc-brrr] [-results ../sc-brrr-results] [-size 10k] [-push] [-watch 5m] [-listen 127.0.0.1:8080]
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Outcome is score.py's OUTCOME line.
type Outcome struct {
	Outcome string `json:"outcome"` // ok | jobs_failed | rejected | setup_failed
	Reason  string `json:"reason"`
	Result  string `json:"result,omitempty"` // result dir, relative to the results checkout
	Plan    string `json:"plan,omitempty"`
	Host    string `json:"host,omitempty"`
	Run     string `json:"run,omitempty"`
	Log     string `json:"log,omitempty"` // the runner's log, when a run started
}

func (o Outcome) scored() bool { return o.Outcome == "ok" || o.Outcome == "jobs_failed" }

type config struct {
	repo, results, size, state string
	repoURL, resultsURL        string
	push                       bool
}

func main() {
	home, _ := os.UserHomeDir()
	var c config
	var watch time.Duration
	flag.StringVar(&c.repo, "repo", "../sc-brrr", "checkout of the challenge repo (score.py, incoming/)")
	flag.StringVar(&c.results, "results", "../sc-brrr-results", "checkout of the results repo")
	flag.StringVar(&c.repoURL, "repo-url", "https://github.com/btraven00/sc-brrr", "cloned into -repo if it is missing")
	flag.StringVar(&c.resultsURL, "results-url", "https://github.com/btraven00/sc-brrr-results", "cloned into -results if it is missing")
	listen := flag.String("listen", "", "serve the live log and recent outcomes here (e.g. 127.0.0.1:8080)")
	flag.StringVar(&c.size, "size", "10k", "input size to score on")
	flag.StringVar(&c.state, "state", filepath.Join(home, ".local/state/sc-brrr-runner"), "logs and the lock")
	flag.BoolVar(&c.push, "push", false, "push both repos after each entry (without it, everything stays local)")
	flag.DurationVar(&watch, "watch", 0, "keep polling at this interval (0: drain the queue once and exit)")
	flag.Parse()
	for _, p := range []*string{&c.repo, &c.results, &c.state} {
		*p, _ = filepath.Abs(*p)
	}
	if err := os.MkdirAll(filepath.Join(c.state, "logs"), 0o755); err != nil {
		log.Fatal(err)
	}
	unlock, err := lock(filepath.Join(c.state, "lock"))
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	for dir, url := range map[string]string{c.repo: c.repoURL, c.results: c.resultsURL} {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			log.Printf("cloning %s into %s", url, dir)
			if _, err := git(filepath.Dir(dir), "clone", "-q", url, dir); err != nil {
				log.Fatal(err)
			}
		}
	}
	if *listen != "" {
		go serve(*listen)
	}

	for {
		if err := drain(c); err != nil {
			log.Fatal(err) // ponytail: stop on any scorer/infra error; a supervisor (systemd) restarts
		}
		if watch == 0 {
			return
		}
		time.Sleep(watch)
	}
}

// lock takes an exclusive, non-blocking flock: one runner per host.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another sc-brrr-runner holds %s", path)
	}
	return func() { f.Close() }, nil
}

func drain(c config) error {
	for _, dir := range []string{c.repo, c.results} {
		if err := syncRepo(dir); err != nil {
			return err
		}
	}
	queue, err := pending(c.repo)
	if err != nil {
		return err
	}
	log.Printf("%d entr(ies) in incoming/", len(queue))
	for _, entry := range queue {
		if err := score(c, entry); err != nil {
			return fmt.Errorf("%s: %w", entry, err)
		}
	}
	return nil
}

// syncRepo refuses a dirty checkout (commits would sweep up stray changes) and fast-forwards it.
func syncRepo(dir string) error {
	if out, err := git(dir, "status", "--porcelain"); err != nil {
		return err
	} else if out != "" {
		return fmt.Errorf("%s has uncommitted changes:\n%s", dir, out)
	}
	if _, err := git(dir, "rev-parse", "--abbrev-ref", "@{upstream}"); err != nil {
		return nil // no upstream (a local test checkout): nothing to pull
	}
	_, err := git(dir, "pull", "-q", "--ff-only")
	return err
}

// pending lists incoming/<account>/<name>-<ver>.yaml, oldest first by the commit that added it.
func pending(repo string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(repo, "incoming", "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	type item struct {
		rel   string
		added int64
	}
	var items []item
	for _, p := range paths {
		if strings.HasSuffix(p, ".env.yml") || strings.HasSuffix(p, ".outcome.json") {
			continue
		}
		rel, _ := filepath.Rel(repo, p)
		out, err := git(repo, "log", "--diff-filter=A", "--format=%ct", "--", rel)
		if err != nil {
			return nil, err
		}
		lines := strings.Fields(out)
		if len(lines) == 0 {
			continue // not committed yet: not queued
		}
		t, _ := strconv.ParseInt(lines[len(lines)-1], 10, 64)
		items = append(items, item{rel, t})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].added != items[j].added {
			return items[i].added < items[j].added
		}
		return items[i].rel < items[j].rel
	})
	queue := make([]string, len(items))
	for i, it := range items {
		queue[i] = it.rel
	}
	return queue, nil
}

func score(c config, entry string) error {
	stem := strings.TrimSuffix(strings.TrimPrefix(entry, "incoming/"), ".yaml") // <account>/<name>-<ver>
	logPath := filepath.Join(c.state, "logs", time.Now().Format("20060102T150405")+"-"+strings.ReplaceAll(stem, "/", "_")+".log")
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	log.Printf("scoring %s (log %s)", entry, logPath)
	live.start(stem, logPath)

	tail := &tailWriter{max: 80}
	cmd := exec.Command("./score.py", "--no-commit", "--size", c.size, "--results", c.results, entry)
	cmd.Dir = c.repo
	cmd.Stdout = io.MultiWriter(lf, os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(lf, os.Stderr, tail)
	runErr := cmd.Run()
	o, ok := tail.outcome()
	live.finish(stem, o, ok)
	if !ok {
		return fmt.Errorf("score.py gave no OUTCOME line (%v); entry left in incoming/, see %s", runErr, logPath)
	}
	log.Printf("%s: %s: %s", entry, o.Outcome, o.Reason)

	if o.scored() {
		if err := commitResults(c, o); err != nil {
			return err
		}
		if err := move(c.repo, entry, "submissions", nil); err != nil {
			return err
		}
		if _, err := git(c.repo, "commit", "-q", "-m", fmt.Sprintf("scored: %s: %s (results: %s)", stem, o.Outcome, o.Result)); err != nil {
			return err
		}
	} else {
		record := struct {
			Outcome
			Time    string   `json:"time"`
			LogTail []string `json:"log_tail"`
		}{o, time.Now().Format(time.RFC3339), tail.lines}
		if o.Log != "" { // the runner's own log says more than score.py's output
			record.LogTail = lastLines(o.Log, 80)
		}
		record.Log = "" // a path on the scoring host; the record is published
		b, _ := json.MarshalIndent(record, "", " ")
		if err := move(c.repo, entry, "failed", b); err != nil {
			return err
		}
		if _, err := git(c.repo, "commit", "-q", "-m", fmt.Sprintf("failed: %s: %s", stem, o.Outcome)); err != nil {
			return err
		}
	}
	if c.push {
		if o.scored() {
			if _, err := git(c.results, "push", "-q"); err != nil {
				return err
			}
		}
		if _, err := git(c.repo, "push", "-q"); err != nil {
			return err
		}
	}
	return nil
}

// commitResults commits the new result, rebuilds the scoreboard and commits that.
func commitResults(c config, o Outcome) error {
	if _, err := git(c.results, "add", "--", o.Result); err != nil {
		return err
	}
	if _, err := git(c.results, "commit", "-q", "-m", o.Reason); err != nil {
		return err
	}
	cmd := exec.Command("./scoreboard.py", c.results)
	cmd.Dir, cmd.Stdout, cmd.Stderr = c.repo, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scoreboard: %w", err)
	}
	if _, err := git(c.results, "add", "-A"); err != nil {
		return err
	}
	if out, _ := git(c.results, "status", "--porcelain"); out == "" {
		return nil
	}
	_, err := git(c.results, "commit", "-q", "-m", "scoreboard: "+o.Result)
	return err
}

// move git-mvs incoming/<acct>/<stem>.yaml and .env.yml to <to>/<acct>/, plus an outcome file if given.
func move(repo, entry, to string, outcome []byte) error {
	dst := filepath.Join(to, strings.TrimPrefix(entry, "incoming/"))
	if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	for _, sfx := range []string{".yaml", ".env.yml"} {
		src, d := strings.TrimSuffix(entry, ".yaml")+sfx, strings.TrimSuffix(dst, ".yaml")+sfx
		if _, err := os.Stat(filepath.Join(repo, src)); errors.Is(err, os.ErrNotExist) {
			continue // a rejected entry may lack its env file
		}
		if _, err := git(repo, "mv", src, d); err != nil {
			return err
		}
	}
	os.Remove(filepath.Join(repo, filepath.Dir(entry))) // the account's incoming dir, if now empty
	if outcome == nil {
		return nil
	}
	f := strings.TrimSuffix(dst, ".yaml") + ".outcome.json"
	if err := os.WriteFile(filepath.Join(repo, f), append(outcome, '\n'), 0o644); err != nil {
		return err
	}
	_, err := git(repo, "add", "--", f)
	return err
}

func git(dir string, args ...string) (string, error) {
	pre := []string{"-C", dir}
	if os.Getenv("GH_TOKEN") != "" { // a token for pushing; never on the command line or in a URL
		pre = append(pre, "-c", "credential.helper=", "-c",
			`credential.helper=!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f`)
	}
	out, err := exec.Command("git", append(pre, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// tailWriter keeps the last max lines written to it, for the OUTCOME line and failure records.
type tailWriter struct {
	max     int
	lines   []string
	partial string
}

func (t *tailWriter) Write(p []byte) (int, error) {
	s := t.partial + string(p)
	parts := strings.Split(s, "\n")
	t.partial = parts[len(parts)-1]
	for _, l := range parts[:len(parts)-1] {
		t.lines = append(t.lines, l)
		if len(t.lines) > t.max {
			t.lines = t.lines[1:]
		}
	}
	return len(p), nil
}

// outcome is the last OUTCOME line seen.
func (t *tailWriter) outcome() (Outcome, bool) {
	var o Outcome
	for i := len(t.lines) - 1; i >= 0; i-- {
		if rest, found := strings.CutPrefix(t.lines[i], "OUTCOME "); found {
			return o, json.Unmarshal([]byte(rest), &o) == nil && o.Outcome != ""
		}
	}
	return o, false
}

func lastLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	t := &tailWriter{max: n}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		t.Write([]byte(sc.Text() + "\n"))
	}
	return t.lines
}

// live is what -listen shows: the running entry's log as it grows, and the recent outcomes.
var live = &status{}

type status struct {
	mu      sync.Mutex
	entry   string // running now, "" when idle
	logPath string
	recent  []string // newest first
}

func (s *status) start(entry, logPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry, s.logPath = entry, logPath
}

func (s *status) finish(entry string, o Outcome, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := fmt.Sprintf("%s  %s  %s", time.Now().Format(time.DateTime), entry, o.Outcome+" ")
	if !ok {
		line += "scorer error (no OUTCOME line)"
	}
	s.recent = append([]string{line}, s.recent...)[:min(len(s.recent)+1, 50)]
	s.entry = ""
}

func (s *status) get() (string, string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entry, s.logPath, append([]string(nil), s.recent...)
}

func serve(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		entry, _, recent := live.get()
		now := "idle"
		if entry != "" {
			now = "scoring " + html.EscapeString(entry) + ` · <a href="/log">live log</a>`
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>sc-brrr runner</title>
<body style="background:#fbf8ef;color:#23211c;font:13px/1.5 ui-monospace,monospace;padding:16px">
<h1 style="font-size:16px">sc-brrr runner</h1><p>%s</p><h2 style="font-size:13px">recent</h2><pre>%s</pre>`,
			now, html.EscapeString(strings.Join(recent, "\n")))
	})
	// /log streams the running entry's log: what is there, then whatever is appended, until the entry finishes
	http.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		entry, path, _ := live.get()
		if entry == "" {
			http.Error(w, "idle: nothing is running", http.StatusNotFound)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		flusher, _ := w.(http.Flusher)
		for {
			if _, err := io.Copy(w, f); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if e, _, _ := live.get(); e != entry {
				io.Copy(w, f) // the last lines
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	})
	log.Printf("live log on http://%s/", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
