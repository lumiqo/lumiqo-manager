package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const distributionRepo = "lumiqot-linux"

type distributionReport struct {
	LifecycleProtocol int                 `json:"lifecycle_protocol"`
	InstalledVersion  string              `json:"installed_version,omitempty"`
	TargetVersion     string              `json:"target_version,omitempty"`
	RollbackVersion   string              `json:"rollback_version,omitempty"`
	Integrity         string              `json:"integrity"`
	IntegrityError    string              `json:"integrity_error,omitempty"`
	Lifecycle         json.RawMessage     `json:"lifecycle,omitempty"`
	LifecycleError    string              `json:"lifecycle_error,omitempty"`
	Pending           *packageTransaction `json:"pending,omitempty"`
	LastError         string              `json:"last_error,omitempty"`
	RecoveryError     string              `json:"recovery_error,omitempty"`
	ReleaseError      string              `json:"release_error,omitempty"`
	HoldRelease       string              `json:"hold_release,omitempty"`
}

func (s distributionStore) report() (distributionReport, error) {
	report := distributionReport{Integrity: "absent", LifecycleProtocol: lifecycleProtocol}
	current, err := s.current()
	if err != nil {
		return report, err
	}
	state, err := s.state()
	if err != nil {
		return report, err
	}
	report.Pending, report.LastError, report.HoldRelease = state.Pending, state.LastError, state.HoldRelease
	report.RecoveryError = state.RecoveryError
	report.InstalledVersion = packageVersion(current)
	report.RollbackVersion = packageVersion(state.Previous)
	if current == "" {
		return report, nil
	}
	report.Integrity = "verified"
	if err := verifyDistribution(s.versionDir(current), report.InstalledVersion, s.arch, false); err != nil {
		report.Integrity, report.IntegrityError = "damaged", err.Error()
	}
	report.Lifecycle, _, err = s.lifecycleStatus(current)
	if err != nil {
		report.LifecycleError = err.Error()
	}
	return report, nil
}

func inspectDistribution(selected, apply bool) (component, error) {
	store := managedDistribution()
	c := component{name: "distribution", policy: "optional", selected: selected}
	report, err := store.report()
	if err != nil {
		return c, err
	}
	c.distribution = &report
	c.present = report.InstalledVersion != "" || report.Pending != nil
	if !selected && !c.present {
		return c, nil
	}

	release, releaseErr := latestDistributionRelease(&http.Client{Timeout: 2 * time.Minute},
		distributionReleaseAPI, distributionReleaseAssets, store.arch)
	report.TargetVersion = release.tag
	c.currentVersion, c.targetVersion = report.InstalledVersion, report.TargetVersion
	if releaseErr != nil {
		report.ReleaseError = releaseErr.Error()
	}
	var life struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(report.Lifecycle, &life)
	switch {
	case report.Pending != nil:
		c.action = "recover"
	case !c.present:
		c.action = "install"
	case report.Integrity != "verified":
		c.action = "repair"
	case releaseErr == nil && shouldUpdate(report.InstalledVersion, release.tag, report.HoldRelease, selected):
		c.action = "update"
	case !store.linkReady():
		c.action = "repair"
	case !lifecycleReady(life.State):
		c.action = "repair"
	default:
		c.ready = true
	}
	if apply {
		c.install = func() error {
			if err := store.configureRetention(); err != nil {
				return err
			}
			if os.Geteuid() != 0 {
				return errors.New("package reconciliation requires root")
			}
			return store.locked(func(store distributionStore) error {
				if err := prepareUserManager(); err != nil {
					return err
				}
				return store.apply(release, releaseErr, selected)
			})
		}
	}
	return c, nil
}

func shouldUpdate(current, available, hold string, explicit bool) bool {
	return available != "" && (current == "" || distributionVersionNewer(available, current)) &&
		(explicit || hold == "" || distributionVersionNewer(available, hold))
}

const distributionConfig = "/etc/lumiqo/distribution.json"

func (s *distributionStore) configureRetention() error {
	file, err := os.Open(distributionConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var config struct {
		RetainVersions int `json:"retain_versions"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return fmt.Errorf("%s: %w", distributionConfig, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || config.RetainVersions < 2 || config.RetainVersions > 20 {
		return fmt.Errorf("%s: retain_versions must be an integer from 2 to 20", distributionConfig)
	}
	s.retain = config.RetainVersions
	return nil
}

func (s distributionStore) apply(release distributionRelease, releaseErr error, explicit bool) error {
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Pending != nil {
		if err := s.recoverPending(state); err != nil {
			return err
		}
		state.Pending = nil
		// Finish recovery on this invocation without immediately retrying the
		// transaction that was just interrupted.
		current, err := s.current()
		if err != nil {
			return err
		}
		return s.cleanup(current, state.Previous)
	}
	current, err := s.current()
	if err != nil {
		return err
	}
	version := packageVersion(current)
	valid := current != "" && verifyDistribution(s.versionDir(current), version, s.arch, false) == nil
	if valid && !shouldUpdate(version, release.tag, state.HoldRelease, explicit) {
		_, phase, _ := s.lifecycleStatus(current)
		if !lifecycleReady(phase) {
			if err := s.start(current); err != nil {
				return err
			}
		}
		if err := s.ensureLink(); err != nil {
			return err
		}
		return s.cleanup(current, state.Previous)
	}
	if releaseErr != nil {
		return releaseErr
	}
	if current != "" && !shouldUpdate(version, release.tag, state.HoldRelease, explicit) {
		release.tag = version
	}
	if current != "" {
		if err := verifyDistribution(s.versionDir(current), version, s.arch, true); err != nil {
			return fmt.Errorf("active CLI cannot safely orchestrate an update: %w", err)
		}
	}
	target, err := s.stage(release)
	if err != nil {
		return err
	}
	return s.activate(state, current, target, false)
}

func rollbackDistribution() error {
	if os.Geteuid() != 0 {
		return errors.New("rollback requires root")
	}
	store := managedDistribution()
	if err := store.configureRetention(); err != nil {
		return err
	}
	return store.locked(func(store distributionStore) error {
		if err := prepareUserManager(); err != nil {
			return err
		}
		return store.rollback()
	})
}

func (s distributionStore) rollback() error {
	state, err := s.state()
	if err != nil {
		return err
	}
	if state.Pending != nil {
		if err := s.recoverPending(state); err != nil {
			return err
		}
		return nil
	}
	current, err := s.current()
	if err != nil {
		return err
	}
	if current == "" || state.Previous == "" {
		return errors.New("no previous working package to roll back to")
	}
	if err := verifyDistribution(s.versionDir(state.Previous), packageVersion(state.Previous), s.arch, false); err != nil {
		return fmt.Errorf("rollback target is damaged: %w", err)
	}
	return s.activate(state, current, state.Previous, true)
}
