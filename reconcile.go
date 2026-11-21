package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const componentHistoryPath = "/var/lib/lumiqo/manager/installed-components.json"
const hushLoginPath = "/home/lumiqo/.hushlogin"

type installedComponents map[string]bool

func installOptional(repo string, stdin io.Reader, stderr io.Writer) (reconcileReport, error) {
	if repo != distributionRepo {
		return reconcileReport{}, fmt.Errorf("unsupported optional repository %q", repo)
	}
	if os.Geteuid() != 0 {
		return reconcileReport{}, errors.New("install requires root")
	}
	return reconcile(true, false, repo, stdin, stderr)
}

type component struct {
	name, policy                  string
	selected                      bool
	currentVersion, targetVersion string
	present, ready                bool
	missingPackages               []string
	install                       func() error
	action                        string
	distribution                  *distributionReport
}

type componentDifference struct {
	Name                string              `json:"name"`
	Policy              string              `json:"policy"`
	Present             bool                `json:"present"`
	Ready               bool                `json:"ready"`
	PreviouslyInstalled bool                `json:"previously_installed"`
	Action              string              `json:"action"`
	MissingPackages     []string            `json:"missing_packages,omitempty"`
	CurrentVersion      string              `json:"current_version,omitempty"`
	TargetVersion       string              `json:"target_version,omitempty"`
	Distribution        *distributionReport `json:"distribution,omitempty"`
}

type reconcileReport struct {
	Restore    bool                  `json:"restore"`
	Components []componentDifference `json:"components"`
	Applied    []string              `json:"applied"`
}

func reconcile(apply, restore bool, repo string, stdin io.Reader, stderr io.Writer) (reconcileReport, error) {
	run := func(history installedComponents, save func() error) (reconcileReport, error) {
		aptMissing := missingPackages()
		_, envErr := os.Stat(environmentRoot + "/current")
		envReady := managedEnvironmentReady()
		hushLogin, hushErr := os.Stat(hushLoginPath)
		components := []component{
			{name: "apt", policy: "required", present: len(aptMissing) == 0, ready: len(aptMissing) == 0, missingPackages: aptMissing, install: func() error {
				_, err := setup(true, stdin, stderr)
				return err
			}},
			{name: "hushlogin", policy: "required", present: hushErr == nil, ready: hushErr == nil && hushLogin.Mode().IsRegular(), install: func() error {
				cmd := exec.Command("/usr/sbin/runuser", "-u", "lumiqo", "--", "/bin/sh", "-ec",
					`[ -f "$1" ] || (set -C; : > "$1")`, "sh", hushLoginPath)
				cmd.Stderr = stderr
				return cmd.Run()
			}},
			{name: "environment", policy: "recommended", present: envErr == nil || envReady, ready: envReady, install: func() error {
				return provisionEnvironment(stderr)
			}},
		}
		c, err := inspectDistribution(repo == distributionRepo, apply)
		if err != nil {
			if !apply {
				return reconcileReport{}, err
			}
			c.selected = true
			c.install = func() error { return err }
		}
		components = append(components, c)
		report := planComponents(components, history, restore)
		if !apply {
			return report, nil
		}
		return applyComponents(report, components, history, save)
	}
	if !apply {
		history, err := readInstalledComponents(componentHistoryPath)
		if err != nil {
			return reconcileReport{}, err
		}
		return run(history, nil)
	}
	return withComponentHistory(componentHistoryPath, run)
}

func planComponents(components []component, history installedComponents, restore bool) reconcileReport {
	report := reconcileReport{Restore: restore, Components: make([]componentDifference, 0, len(components)), Applied: []string{}}
	for _, c := range components {
		action := "none"
		switch {
		case c.ready:
		case c.policy == "optional" && !c.present && !c.selected:
			action = "skip_optional"
		case c.policy == "recommended" && !c.present && history[c.name] && !restore:
			action = "skip_previously_installed"
		case c.action != "":
			action = c.action
		default:
			action = "install"
		}
		report.Components = append(report.Components, componentDifference{
			Name: c.name, Policy: c.policy, Present: c.present, Ready: c.ready,
			PreviouslyInstalled: history[c.name], Action: action, MissingPackages: c.missingPackages,
			CurrentVersion: c.currentVersion, TargetVersion: c.targetVersion, Distribution: c.distribution,
		})
	}
	return report
}

func applyComponents(report reconcileReport, components []component, history installedComponents, save func() error) (reconcileReport, error) {
	for i, c := range components {
		switch report.Components[i].Action {
		case "install", "repair", "update", "recover":
			if err := c.install(); err != nil {
				return report, fmt.Errorf("reconcile %s: %w", c.name, err)
			}
			report.Applied = append(report.Applied, c.name)
			if report.Components[i].Action == "recover" && c.distribution.Pending.Old == "" && !history[c.name] {
				continue
			}
		case "none":
		default:
			continue
		}
		if err := recordComponent(history, save, c.name); err != nil {
			return report, err
		}
	}
	return report, nil
}

func recordComponent(history installedComponents, save func() error, name string) error {
	if history[name] {
		return nil
	}
	history[name] = true
	if err := save(); err != nil {
		delete(history, name)
		return fmt.Errorf("record %s installation: %w", name, err)
	}
	return nil
}

func withComponentHistory[T any](path string, run func(installedComponents, func() error) (T, error)) (T, error) {
	var zero T
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return zero, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".reconcile.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return zero, err
	}
	history, err := readInstalledComponents(path)
	if err != nil {
		return zero, err
	}
	return run(history, func() error { return writeInstalledComponents(path, history) })
}

func readInstalledComponents(path string) (installedComponents, error) {
	history := installedComponents{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return history, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &history); err != nil || history == nil {
		return nil, fmt.Errorf("invalid installation history at %s", path)
	}
	return history, nil
}

func writeInstalledComponents(path string, history installedComponents) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := os.CreateTemp(dir.Name(), ".installed-components-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writeErr := json.NewEncoder(file).Encode(history)
	if err := errors.Join(writeErr, file.Chmod(0644), file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return dir.Sync()
}
