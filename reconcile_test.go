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
		{name: "worker", policy: "optional"},
	}
	history := installedComponents{"apt": true, "removed": true, "damaged": true, "worker": true}
	for _, tc := range []struct {
		restore, worker   bool
		removed, optional string
	}{
		{false, false, "skip_previously_installed", "skip_optional"},
		{true, false, "install", "skip_optional"},
		{false, true, "skip_previously_installed", "install"},
		{true, true, "install", "install"},
	} {
		components[5].selected = tc.worker
		report := planComponents(components, history, tc.restore)
		var got []string
		for _, diff := range report.Components {
			got = append(got, diff.Action)
		}
		want := []string{"install", "install", tc.removed, "install", "none", tc.optional}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("restore=%v worker=%v: actions %v, want %v", tc.restore, tc.worker, got, want)
		}
	}
}

func TestReconcileUpdatesPreviouslyInstalledWorker(t *testing.T) {
	worker := component{name: "worker", policy: "optional", present: true, currentVersion: "v1.9.0", targetVersion: "v1.10.0"}
	history := installedComponents{}
	report := planComponents([]component{worker}, history, false)
	diff := report.Components[0]
	if diff.Action != "install" || diff.CurrentVersion != "v1.9.0" || diff.TargetVersion != "v1.10.0" {
		t.Fatalf("unexpected Worker diff: %+v", diff)
	}
	worker.currentVersion = worker.targetVersion
	worker.ready = true
	if got := planComponents([]component{worker}, history, false).Components[0].Action; got != "none" {
		t.Fatalf("current Worker action = %q", got)
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
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || len(report.Components) != 3 || len(report.Applied) != 0 {
		t.Fatalf("unexpected plan %q, err = %v", stdout.String(), err)
	}
	for _, args := range [][]string{{}, {"--plan", "--apply"}, {"--plan", "--restore"}, {"--restore"}} {
		if err := run(append([]string{"reconcile"}, args...), nil, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted invalid options %v", args)
		}
	}
}

func TestInstallWorkerUnitPreservesExistingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), workerUnit)
	if err := installWorkerUnit(path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != unitContents {
		t.Fatalf("new unit = %q, err = %v", data, err)
	}
	const custom = "[Service]\nEnvironment=EXAMPLE=custom\n"
	if err := os.WriteFile(path, []byte(custom), 0644); err != nil {
		t.Fatal(err)
	}
	if err := installWorkerUnit(path); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != custom {
		t.Fatalf("existing unit = %q, err = %v", data, err)
	}
}
