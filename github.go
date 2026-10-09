package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// github is the REST API as the runner uses it: find an entry's PR, comment on it, edit the comment.
type github struct{ token, owner, repo string }

var ghRepo = regexp.MustCompile(`^https://github\.com/([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

func newGitHub(token, repoURL string) *github {
	m := ghRepo.FindStringSubmatch(repoURL)
	if token == "" || m == nil {
		return nil
	}
	return &github{token, m[1], m[2]}
}

func (g *github) api(method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// announce comments on the PR that added the entry, linking the live log. GitHub trouble never
// stops scoring: it is logged, and the comment is skipped.
func (c config) announce(e queued, stem, logName string) (pr int, commentID int64) {
	if c.gh == nil {
		return 0, 0
	}
	var prs []struct {
		Number int `json:"number"`
	}
	if err := c.gh.api("GET", fmt.Sprintf("/repos/%s/%s/commits/%s/pulls", c.gh.owner, c.gh.repo, e.sha), nil, &prs); err != nil || len(prs) == 0 {
		log.Printf("warning: no PR found for %s (commit %.7s): %v", stem, e.sha, err)
		return 0, 0
	}
	var made struct {
		ID int64 `json:"id"`
	}
	body := fmt.Sprintf("⏳ **Scoring `%s`** at %s (each size in turn, stopping at the first that doesn't score ok). %s",
		stem, strings.Join(c.ladder(), ", "), c.logLink(logName, "Live log"))
	if err := c.gh.api("POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.gh.owner, c.gh.repo, prs[0].Number),
		map[string]string{"body": body}, &made); err != nil {
		log.Printf("warning: commenting on PR #%d: %v", prs[0].Number, err)
		return prs[0].Number, 0
	}
	return prs[0].Number, made.ID
}

// report edits the announcement with each size's outcome and where to find its result or the reason.
func (c config) report(pr int, commentID int64, stem string, steps []step) {
	if c.gh == nil || commentID == 0 || len(steps) == 0 {
		return
	}
	results := strings.TrimSuffix(strings.TrimSuffix(c.resultsURL, "/"), ".git")
	repo := strings.TrimSuffix(strings.TrimSuffix(c.repoURL, "/"), ".git")
	icon := map[string]string{"ok": "✅", "jobs_failed": "❌", "rejected": "⛔", "setup_failed": "⛔"}
	var lines []string
	for _, st := range steps {
		where := ""
		switch {
		case st.o.scored():
			where = fmt.Sprintf("[result](%s/tree/HEAD/%s)", results, st.o.Result)
		case st == steps[0]:
			where = fmt.Sprintf("not scored: [reason and log](%s/blob/HEAD/failed/%s.outcome.json)", repo, stem)
		default:
			where = "not scored"
		}
		lines = append(lines, fmt.Sprintf("| %s | %s %s | %s | %s |", st.size, icon[st.o.Outcome], st.o.Outcome, where, c.logLink(st.logName, "log")))
	}
	last := steps[len(steps)-1].o
	note := ""
	if last.Outcome != "ok" {
		note = fmt.Sprintf("\n\nStopped at %s:\n```text\n%s\n```", steps[len(steps)-1].size, strings.ReplaceAll(c.redact.Replace(last.Reason), "```", "'''"))
	}
	if !c.push {
		note += "\n\n(not pushed yet)"
	}
	body := fmt.Sprintf("**`%s`**\n\n| size | outcome | | |\n|---|---|---|---|\n%s%s", stem, strings.Join(lines, "\n"), note)
	if err := c.gh.api("PATCH", fmt.Sprintf("/repos/%s/%s/issues/comments/%d", c.gh.owner, c.gh.repo, commentID),
		map[string]string{"body": body}, nil); err != nil {
		log.Printf("warning: updating the comment on PR #%d: %v", pr, err)
	}
}

// logLink points at a kept log, or at the live page when logName is empty.
func (c config) logLink(logName, label string) string {
	if c.publicURL == "" {
		return label + ": kept on the scoring host"
	}
	if logName == "" {
		return fmt.Sprintf("[%s](%s/)", label, strings.TrimSuffix(c.publicURL, "/"))
	}
	return fmt.Sprintf("[%s](%s/logs/%s)", label, strings.TrimSuffix(c.publicURL, "/"), logName)
}
