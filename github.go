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
	body := fmt.Sprintf("⏳ **Scoring `%s`** on the %s input. %s", stem, c.size, c.logLink(logName, "Live log"))
	if err := c.gh.api("POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.gh.owner, c.gh.repo, prs[0].Number),
		map[string]string{"body": body}, &made); err != nil {
		log.Printf("warning: commenting on PR #%d: %v", prs[0].Number, err)
		return prs[0].Number, 0
	}
	return prs[0].Number, made.ID
}

// report edits the announcement with the outcome and where to find the result or the reason.
func (c config) report(pr int, commentID int64, stem, logName string, o Outcome) {
	if c.gh == nil || commentID == 0 {
		return
	}
	icon, where := "❌", ""
	results := strings.TrimSuffix(strings.TrimSuffix(c.resultsURL, "/"), ".git")
	repo := strings.TrimSuffix(strings.TrimSuffix(c.repoURL, "/"), ".git")
	switch {
	case o.scored():
		if o.Outcome == "ok" {
			icon = "✅"
		}
		where = fmt.Sprintf("Result: %s/tree/HEAD/%s", results, o.Result)
	case o.Outcome == "rejected" || o.Outcome == "setup_failed":
		where = fmt.Sprintf("Not scored; the reason and the end of the log: %s/blob/HEAD/failed/%s.outcome.json", repo, stem)
	}
	if !c.push {
		where += " (not pushed yet)"
	}
	body := fmt.Sprintf("%s **`%s`: %s**\n\n```text\n%s\n```\n\n%s\n\n%s", icon, stem, o.Outcome,
		strings.ReplaceAll(o.Reason, "```", "'''"), where, c.logLink(logName, "Log"))
	if err := c.gh.api("PATCH", fmt.Sprintf("/repos/%s/%s/issues/comments/%d", c.gh.owner, c.gh.repo, commentID),
		map[string]string{"body": body}, nil); err != nil {
		log.Printf("warning: updating the comment on PR #%d: %v", pr, err)
	}
}

func (c config) logLink(logName, label string) string {
	if c.publicURL == "" {
		return label + ": kept on the scoring host (no public-url set)."
	}
	return fmt.Sprintf("%s: %s/logs/%s", label, strings.TrimSuffix(c.publicURL, "/"), logName)
}
