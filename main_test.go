package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestSetupRejectsPlanWithApply(t *testing.T) {
	var stdout bytes.Buffer
	err := run([]string{"setup", "--plan", "--apply"}, nil, &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") || stdout.Len() != 0 {
		t.Fatalf("expected error and no report, got %v and %q", err, stdout.String())
	}
}

func TestToolsListJSON(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stdout bytes.Buffer
	if err := run([]string{"tools", "list", "--json"}, nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var tools map[string]bool
	if err := json.Unmarshal(stdout.Bytes(), &tools); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex", "gh", "git", "ntn", "gcloud", "mise", "uv", "docker"} {
		if installed, reported := tools[name]; !reported || installed {
			t.Errorf("expected %s to be reported as missing, got %v", name, tools)
		}
	}
}
