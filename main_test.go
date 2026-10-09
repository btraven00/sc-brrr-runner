package main

import (
	"os"
	"testing"
)

// The OUTCOME line is the whole contract with score.py: the last one wins, split writes reassemble,
// and output without one is an error of the scorer.
func TestOutcome(t *testing.T) {
	w := &tailWriter{max: 3}
	w.Write([]byte("runner noise\nOUTCOME {\"outcome\": \"rejected\", \"reason\": \"old\"}\nmore\n"))
	w.Write([]byte("OUTCOME {\"outcome\": \"jobs_failed\", \"reason\": \"x\", \"res"))
	w.Write([]byte("ult\": \"a/b/0.1.0/p/h/10k\"}\n"))
	o, ok := w.outcome()
	if !ok || o.Outcome != "jobs_failed" || o.Result != "a/b/0.1.0/p/h/10k" || !o.scored() {
		t.Fatalf("got %+v, %v", o, ok)
	}
	if len(w.lines) != 3 {
		t.Fatalf("tail kept %d lines, want 3", len(w.lines))
	}
	if _, ok := (&tailWriter{max: 5}).outcome(); ok {
		t.Fatal("no OUTCOME line must not parse")
	}
	w = &tailWriter{max: 5}
	w.Write([]byte("OUTCOME not json\n"))
	if _, ok := w.outcome(); ok {
		t.Fatal("a malformed OUTCOME line must not parse")
	}
}

// The config file is flat YAML: comments, quoted values, and values that contain colons (URLs).
func TestReadConfig(t *testing.T) {
	f := t.TempDir() + "/c.yaml"
	os.WriteFile(f, []byte(`# runner config
github-token: "ghp_abc#def"   
public-url: https://runner.example.org:8443  # trailing comment
size: '50k'
push: true
`), 0o600)
	cfg, err := readConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"github-token": "ghp_abc#def", "public-url": "https://runner.example.org:8443", "size": "50k", "push": "true"}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("%s = %q, want %q", k, cfg[k], v)
		}
	}
	if cfg, err := readConfig(t.TempDir() + "/missing.yaml"); cfg != nil || err != nil {
		t.Errorf("a missing config is no config: %v %v", cfg, err)
	}
}
