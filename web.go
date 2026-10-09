package main

import (
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// live is what -listen shows: the running entry and the recent outcomes. Logs themselves are the
// files in the state dir, kept for good.
var live = &status{}

type status struct {
	mu      sync.Mutex
	entry   string // running now, "" when idle
	logName string
	recent  []string // newest first
}

func (s *status) start(entry, logName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entry, s.logName = entry, logName
}

func (s *status) finish(entry string, o Outcome, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := o.Outcome
	if !ok {
		out = "scorer error (no OUTCOME line)"
	}
	s.recent = append([]string{fmt.Sprintf("%s  %s  %s", time.Now().Format(time.DateTime), entry, out)}, s.recent...)
	s.recent = s.recent[:min(len(s.recent), 50)]
	s.entry = ""
}

func (s *status) get() (entry, logName string, recent []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entry, s.logName, append([]string(nil), s.recent...)
}

var logFile = regexp.MustCompile(`^[\w.-]+\.log$`) // names the runner makes; nothing else is served

const page = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>sc-brrr runner</title>
<style>body{background:#fbf8ef;color:#23211c;font:13px/1.5 ui-monospace,Menlo,monospace;margin:0 auto;max-width:960px;padding:24px 16px}
h1{font-size:16px}h2{font-size:13px;color:#77725f;margin-top:24px}a{color:#23211c}pre{white-space:pre-wrap}</style>
<h1>sc-brrr runner</h1>`

func serve(addr, logDir string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		entry, name, recent := live.get()
		now := "idle"
		if entry != "" {
			now = fmt.Sprintf(`scoring %s · <a href="/logs/%s">live log</a>`, html.EscapeString(entry), name)
		}
		var logs []string
		if des, err := os.ReadDir(logDir); err == nil {
			for _, d := range des {
				if logFile.MatchString(d.Name()) {
					logs = append(logs, d.Name())
				}
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(logs))) // names start with the time: newest first
		var b strings.Builder
		for _, l := range logs[:min(len(logs), 200)] {
			fmt.Fprintf(&b, "<a href=\"/logs/%s\">%s</a>\n", l, l)
		}
		fmt.Fprintf(w, "%s<p>%s</p><h2>recent outcomes (since the runner started)</h2><pre>%s</pre><h2>logs</h2><pre>%s</pre>",
			page, now, html.EscapeString(strings.Join(recent, "\n")), b.String())
	})
	// a finished log is a file; the running entry's log streams as it grows, until the entry finishes
	mux.HandleFunc("GET /logs/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !logFile.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(logDir, name))
		if err != nil {
			http.NotFound(w, r)
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
			if entry, running, _ := live.get(); entry == "" || running != name {
				io.Copy(w, f) // whatever came last
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	})
	log.Printf("live page on http://%s/", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
