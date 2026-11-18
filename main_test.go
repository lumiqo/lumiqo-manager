package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

func TestWorkerStatusMatchesServices(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	systemctl := `#!/bin/sh
test "$#" = 5 && test "$1" = "--user" && test "$2" = "show" &&
    test "$4" = "--" && test "$5" = "test-worker.service" || exit 1
printf '%s\n' LoadState=loaded ActiveState=active SubState=running
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctl), 0700); err != nil {
		t.Fatal(err)
	}
	want := service{Unit: "test-worker.service", Scope: "user", Load: "loaded", Active: "active", Sub: "running"}
	for _, command := range [][]string{{"services"}, {"worker", "status"}} {
		args := append(command, "--user", "--unit", want.Unit, "--json")
		var stdout bytes.Buffer
		if err := run(args, nil, &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		var got service
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%v returned %+v, want %+v", command, got, want)
		}
	}
}
