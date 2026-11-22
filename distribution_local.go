package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func installLocalDistribution(tag, digest string, archive io.Reader) (distributionReport, error) {
	var report distributionReport
	sum, err := hex.DecodeString(digest)
	if !stableDistributionTag.MatchString(tag) || err != nil || len(sum) != 32 {
		return report, errors.New("local archive requires a stable version and a SHA-256 digest")
	}
	if os.Geteuid() != 0 {
		return report, errors.New("local archive installation requires root")
	}
	store := managedDistribution()
	if err := store.configureRetention(); err != nil {
		return report, err
	}
	err = store.locked(func(store distributionStore) error {
		if err := prepareUserManager(); err != nil {
			return err
		}
		state, err := store.state()
		if err != nil {
			return err
		}
		if state.Pending != nil {
			if err := store.recoverPending(state); err != nil {
				return err
			}
			return errors.New("recovered an interrupted package transaction; retry the local install")
		}
		current, err := store.current()
		if err != nil {
			return err
		}
		if current != "" {
			if err := verifyDistribution(store.versionDir(current), packageVersion(current), store.arch, false); err != nil {
				return fmt.Errorf("cannot verify the active package for rollback: %w", err)
			}
			if _, phase, err := store.lifecycleStatus(current); err != nil || !lifecycleReady(phase) {
				return errors.New("the active services must be healthy or disabled before local installation")
			}
		}
		target, err := store.stageLocal(tag, archive, strings.ToLower(digest))
		if err != nil {
			return err
		}
		if err := store.activate(state, current, target, false); err != nil {
			return err
		}
		report, err = store.report()
		return err
	})
	return report, err
}
