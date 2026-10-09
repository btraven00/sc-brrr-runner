// sc-brrr-runner: the scoring service for the sc-brrr challenge.
//
// It drains the queue in a checkout of sc-brrr: every entry in incoming/<account>/ (a merged,
// reviewed submission) is scored by the challenge repo's own score.py (podman runner, main's
// code), oldest first. Then:
//
//   - scored (jobs ok or failed): the result and its log (score.log) are committed to the results
//     checkout, the scoreboard rebuilt and committed, and the entry moved to submissions/;
//   - not scored (rejected, or setup failed before any job ran): the entry is moved to failed/ with
//     <name>-<ver>.outcome.json (the reason and the end of the log). To retry, git mv it back.
//
// score.py's contract: the last "OUTCOME {json}" line of its output and its exit code. No OUTCOME
// line means the scorer itself broke: the entry stays in incoming/ and the runner stops.
//
// Configuration: flags, or the same keys in sc-brrr-runner.yaml (git-ignored; flags win), plus
// github-token, which is never a flag. With a token, git pushes with it and the runner comments on
// the entry's PR with a link to the live log, then edits the comment with the outcome. Missing
// checkouts are cloned. -listen serves the live page and every kept log.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
	dataCache                  string // the host's input cache (HF cache layout), filled and mounted by runner.py
	repoURL, resultsURL        string
	publicURL                  string // where -listen is reachable from outside, for the PR comment
	push, comment              bool
	gh                         *github // nil without a token, or with comments off
	redact                     *strings.Replacer
}

// token is the GitHub token: git pushes with it; it never reaches score.py or the submissions.
var token string

// gitName, gitEmail: who the runner's commits are by (e.g. the bot account); empty: git's own config.
var gitName, gitEmail string

func main() {
	home, _ := os.UserHomeDir()
	var c config
	var watch time.Duration
	cfgPath := flag.String("config", "sc-brrr-runner.yaml", "config file: flag names as keys, plus github-token (missing: flags only)")
	flag.StringVar(&c.repo, "repo", "../sc-brrr", "checkout of the challenge repo (score.py, incoming/)")
	flag.StringVar(&c.results, "results", "../sc-brrr-results", "checkout of the results repo")
	flag.StringVar(&c.repoURL, "repo-url", "https://github.com/btraven00/sc-brrr", "cloned into -repo if missing; its PRs get the comments")
	flag.StringVar(&c.resultsURL, "results-url", "https://github.com/btraven00/sc-brrr-results", "cloned into -results if missing")
	listen := flag.String("listen", "", "serve the live page and the kept logs here (e.g. 127.0.0.1:8080)")
	flag.StringVar(&c.publicURL, "public-url", "", "the live page's public address, linked from PR comments (e.g. https://runner.example.org)")
	flag.StringVar(&c.size, "size", "10k", "input size to score on")
	flag.StringVar(&c.dataCache, "data-cache", "cache", "input cache: runner.py downloads each input once, checks its sha256, mounts it read-only")
	flag.StringVar(&c.state, "state", filepath.Join(home, ".local/state/sc-brrr-runner"), "kept logs and the lock")
	flag.BoolVar(&c.push, "push", false, "push both repos after each entry (without it, everything stays local)")
	flag.BoolVar(&c.comment, "comment", false, "comment on the entry's PR (live-log link, then the outcome), as the token's owner")
	flag.StringVar(&gitName, "git-name", "", "author and committer of the runner's commits (empty: git's config)")
	flag.StringVar(&gitEmail, "git-email", "", "their email, e.g. <id>+<bot>@users.noreply.github.com")
	flag.DurationVar(&watch, "watch", 0, "keep polling at this interval (0: drain the queue once and exit)")
	flag.Parse()
	if err := applyConfig(*cfgPath); err != nil {
		log.Fatal(err)
	}
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	if c.comment {
		if c.gh = newGitHub(token, c.repoURL); c.gh == nil {
			log.Fatal("-comment needs github-token (config) or $GH_TOKEN, and a github.com -repo-url")
		}
	}
	for _, p := range []*string{&c.repo, &c.results, &c.state, &c.dataCache} {
		*p, _ = filepath.Abs(*p)
	}
	// what gets published (served and committed logs, failure records, PR comments) names no host
	// paths; most specific first, as the replacer tries its pairs in order
	c.redact = strings.NewReplacer(c.repo, "<repo>", c.results, "<results>", c.state, "<state>", c.dataCache, "<data-cache>", home, "~")
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
		go serve(*listen, filepath.Join(c.state, "logs"))
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

// applyConfig sets every flag the command line left alone from the config file, and the token.
func applyConfig(path string) error {
	cfg, err := readConfig(path)
	if err != nil || cfg == nil {
		return err
	}
	onCLI := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { onCLI[f.Name] = true })
	for k, v := range cfg {
		switch {
		case k == "github-token":
			token = v
		case k == "config" || flag.Lookup(k) == nil:
			return fmt.Errorf("%s: unknown key %q (keys are the flag names, plus github-token)", path, k)
		case !onCLI[k]:
			if err := flag.Set(k, v); err != nil {
				return fmt.Errorf("%s: %s: %w", path, k, err)
			}
		}
	}
	return nil
}

// readConfig reads a flat YAML file of "key: value" lines (comments, quoted values). Missing: nil.
// ponytail: flat keys only; a YAML library once the config needs nesting
func readConfig(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		log.Printf("warning: %s is readable by other users and may hold a token: chmod 600 %s", path, path)
	}
	cfg := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want key: value", path, i+1)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case strings.HasPrefix(v, `"`):
			if v, err = strconv.Unquote(v); err != nil {
				return nil, fmt.Errorf("%s:%d: bad double-quoted value", path, i+1)
			}
		case strings.HasPrefix(v, "'"):
			if len(v) < 2 || !strings.HasSuffix(v, "'") {
				return nil, fmt.Errorf("%s:%d: bad single-quoted value", path, i+1)
			}
			v = v[1 : len(v)-1]
		default:
			if j := strings.Index(v, " #"); j >= 0 {
				v = strings.TrimSpace(v[:j])
			}
		}
		cfg[k] = v
	}
	return cfg, nil
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
	for _, e := range queue {
		if err := score(c, e); err != nil {
			return fmt.Errorf("%s: %w", e.rel, err)
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

// queued is an entry in incoming/ and the commit that added it (which leads to its PR).
type queued struct {
	rel, sha string
	added    int64
}

// pending lists incoming/<account>/<name>-<ver>.yaml, oldest first by the commit that added it.
func pending(repo string) ([]queued, error) {
	paths, err := filepath.Glob(filepath.Join(repo, "incoming", "*", "*.yaml"))
	if err != nil {
		return nil, err
	}
	var q []queued
	for _, p := range paths {
		rel, _ := filepath.Rel(repo, p)
		out, err := git(repo, "log", "--diff-filter=A", "--format=%H %ct", "--", rel)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(out, "\n")
		f := strings.Fields(lines[len(lines)-1]) // the first add
		if len(f) != 2 {
			continue // not committed yet: not queued
		}
		t, _ := strconv.ParseInt(f[1], 10, 64)
		q = append(q, queued{rel, f[0], t})
	}
	sort.Slice(q, func(i, j int) bool {
		if q[i].added != q[j].added {
			return q[i].added < q[j].added
		}
		return q[i].rel < q[j].rel
	})
	return q, nil
}

func score(c config, e queued) error {
	stem := strings.TrimSuffix(strings.TrimPrefix(e.rel, "incoming/"), ".yaml") // <account>/<name>-<ver>
	logName := time.Now().Format("20060102T150405") + "-" + strings.ReplaceAll(stem, "/", "_") + ".log"
	logPath := filepath.Join(c.state, "logs", logName)
	lf, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer lf.Close()
	log.Printf("scoring %s (log %s)", e.rel, logPath)
	live.start(stem, logName)
	pr, commentID := c.announce(e, stem, logName)

	tail := &tailWriter{max: 80} // raw: the OUTCOME line's log path is read back
	published := &redactWriter{w: lf, r: c.redact}
	cmd := exec.Command("./score.py", "--no-commit", "--size", c.size, "--results", c.results, e.rel)
	cmd.Dir, cmd.Env = c.repo, append(scrubbedEnv(), "SC_BRRR_DATA_CACHE="+c.dataCache)
	cmd.Stdout = io.MultiWriter(published, os.Stdout, tail)
	cmd.Stderr = io.MultiWriter(published, os.Stderr, tail)
	runErr := cmd.Run()
	published.Flush()
	o, ok := tail.outcome()
	live.finish(stem, o, ok)
	if !ok {
		c.report(pr, commentID, stem, logName, Outcome{Outcome: "scorer error", Reason: "the scorer broke; the entry stays queued"})
		return fmt.Errorf("score.py gave no OUTCOME line (%v); entry left in incoming/, see %s", runErr, logPath)
	}
	log.Printf("%s: %s: %s", e.rel, o.Outcome, o.Reason)

	if o.scored() {
		lf.Sync()
		if err := copyFile(logPath, filepath.Join(c.results, o.Result, "score.log")); err != nil {
			return err
		}
		if err := commitResults(c, o); err != nil {
			return err
		}
		if err := move(c.repo, e.rel, "submissions", nil); err != nil {
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
		record.Reason = c.redact.Replace(record.Reason)
		for i, l := range record.LogTail {
			record.LogTail[i] = c.redact.Replace(l)
		}
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false) // "<repo>", not "\u003crepo\u003e": the record is read by people
		enc.SetIndent("", " ")
		enc.Encode(record)
		if err := move(c.repo, e.rel, "failed", bytes.TrimSpace(b.Bytes())); err != nil {
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
	c.report(pr, commentID, stem, logName, o) // after the push, so its links resolve
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
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = c.repo, scrubbedEnv(), os.Stdout, os.Stderr
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

// scrubbedEnv is the environment for score.py and scoreboard.py: without the token, which would
// otherwise reach the setup step that builds a submission's env with network on.
func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GH_TOKEN=") && !strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			env = append(env, kv)
		}
	}
	return env
}

func git(dir string, args ...string) (string, error) {
	pre := []string{"-C", dir}
	cmd := exec.Command("git")
	cmd.Env = scrubbedEnv()
	if token != "" { // through a credential helper reading the env: never on the command line or in a URL
		pre = append(pre, "-c", "credential.helper=", "-c",
			`credential.helper=!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f`)
		cmd.Env = append(cmd.Env, "GH_TOKEN="+token)
	}
	if gitName != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME="+gitName, "GIT_COMMITTER_NAME="+gitName)
	}
	if gitEmail != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_EMAIL="+gitEmail, "GIT_COMMITTER_EMAIL="+gitEmail)
	}
	cmd.Args = append(cmd.Args, append(pre, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// redactWriter writes whole lines through the replacer, so a path split across writes still matches.
type redactWriter struct {
	w   io.Writer
	r   *strings.Replacer
	buf []byte
}

func (d *redactWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	if i := bytes.LastIndexByte(d.buf, '\n'); i >= 0 {
		if _, err := io.WriteString(d.w, d.r.Replace(string(d.buf[:i+1]))); err != nil {
			return 0, err
		}
		d.buf = append(d.buf[:0], d.buf[i+1:]...)
	}
	return len(p), nil
}

// Flush writes a last line that had no newline.
func (d *redactWriter) Flush() {
	io.WriteString(d.w, d.r.Replace(string(d.buf)))
	d.buf = d.buf[:0]
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
