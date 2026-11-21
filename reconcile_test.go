package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReconcileComponentPolicy(t *testing.T) {
	components := []component{
		{name: "apt", policy: "required"},
		{name: "first", policy: "recommended"},
		{name: "removed", policy: "recommended"},
		{name: "damaged", policy: "recommended", present: true},
		{name: "healthy", policy: "recommended", present: true, ready: true},
		{name: "distribution", policy: "optional"},
	}
	history := installedComponents{"apt": true, "removed": true, "damaged": true, "distribution": true}
	for _, tc := range []struct {
		restore, distribution bool
		removed, optional     string
	}{
		{false, false, "skip_previously_installed", "skip_optional"},
		{true, false, "install", "skip_optional"},
		{false, true, "skip_previously_installed", "install"},
		{true, true, "install", "install"},
	} {
		components[5].selected = tc.distribution
		report := planComponents(components, history, tc.restore)
		var got []string
		for _, diff := range report.Components {
			got = append(got, diff.Action)
		}
		want := []string{"install", "install", tc.removed, "install", "none", tc.optional}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("restore=%v distribution=%v: actions %v, want %v", tc.restore, tc.distribution, got, want)
		}
	}
}

func TestReconcileUpdatesPreviouslyInstalledDistribution(t *testing.T) {
	distribution := component{name: "distribution", policy: "optional", present: true, currentVersion: "v1.9.0", targetVersion: "v1.10.0"}
	history := installedComponents{}
	report := planComponents([]component{distribution}, history, false)
	diff := report.Components[0]
	if diff.Action != "install" || diff.CurrentVersion != "v1.9.0" || diff.TargetVersion != "v1.10.0" {
		t.Fatalf("unexpected Distribution diff: %+v", diff)
	}
	distribution.currentVersion = distribution.targetVersion
	distribution.ready = true
	if got := planComponents([]component{distribution}, history, false).Components[0].Action; got != "none" {
		t.Fatalf("current Distribution action = %q", got)
	}
}

func TestReconcileRetriesFailureAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manager", "installed-components.json")
	attempts := map[string]int{}
	ready := map[string]bool{}
	fail := true
	apply := func() (reconcileReport, error) {
		return withComponentHistory(path, func(history installedComponents, save func() error) (reconcileReport, error) {
			var components []component
			for _, entry := range []struct{ name, policy string }{{"apt", "required"}, {"environment", "recommended"}} {
				components = append(components, component{
					name: entry.name, policy: entry.policy, present: ready[entry.name], ready: ready[entry.name],
					install: func() error {
						attempts[entry.name]++
						if entry.name == "environment" && fail {
							return errors.New("temporary provisioning failure")
						}
						ready[entry.name] = true
						return nil
					},
				})
			}
			return applyComponents(planComponents(components, history, false), components, history, save)
		})
	}
	if _, err := apply(); err == nil {
		t.Fatal("expected provisioning failure")
	}
	history, err := readInstalledComponents(path)
	if err != nil || !reflect.DeepEqual(history, installedComponents{"apt": true}) {
		t.Fatalf("history after failure = %v, err = %v", history, err)
	}

	fail = false
	report, err := apply()
	if err != nil || !reflect.DeepEqual(report.Applied, []string{"environment"}) {
		t.Fatalf("retry applied %v, err = %v", report.Applied, err)
	}
	report, err = apply()
	if err != nil || len(report.Applied) != 0 || attempts["apt"] != 1 || attempts["environment"] != 2 {
		t.Fatalf("third run applied %v, attempts = %v, err = %v", report.Applied, attempts, err)
	}
	history, err = readInstalledComponents(path)
	if err != nil || !reflect.DeepEqual(history, installedComponents{"apt": true, "environment": true}) {
		t.Fatalf("history after retry = %v, err = %v", history, err)
	}
}

func TestReconcileHistoryRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installed-components.json")
	if err := os.WriteFile(path, []byte("null"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := withComponentHistory(path, func(installedComponents, func() error) (bool, error) {
		t.Fatal("apply called with invalid history")
		return false, nil
	}); err == nil {
		t.Fatal("expected corrupt history error")
	}
}

func TestReconcilePlanCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stdout bytes.Buffer
	if err := run([]string{"reconcile", "--plan"}, nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report reconcileReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Components) != 4 || len(report.Applied) != 0 {
		t.Fatalf("unexpected plan %q, err = %v", stdout.String(), err)
	}
	if diff := report.Components[1]; diff.Name != "hushlogin" || diff.Policy != "required" {
		t.Fatalf("unexpected login configuration in plan: %+v", diff)
	}
	for _, args := range [][]string{{}, {"--plan", "--apply"}, {"--plan", "--restore"}, {"--restore"}} {
		if err := run(append([]string{"reconcile"}, args...), nil, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted invalid options %v", args)
		}
	}
}
