package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const distributionRoot = "/opt/lumiqo/distribution"
const distributionLink = "/usr/local/bin/lumiqot"
const distributionUser = "lumiqo"

var installationID = regexp.MustCompile(`^(v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))-[0-9]+$`)

type installation struct {
	Previous      string              `json:"previous,omitempty"`
	HoldRelease   string              `json:"hold_release,omitempty"`
	Pending       *packageTransaction `json:"pending,omitempty"`
	LastError     string              `json:"last_error,omitempty"`
	RecoveryError string              `json:"recovery_error,omitempty"`
}

type packageTransaction struct {
	Old    string `json:"old,omitempty"`
	Target string `json:"target"`
	Phase  string `json:"phase"`
}

type distributionStore struct {
	lock             *os.File
	root, link, arch string
	retain           int
	call             func(id, action string, seconds int) ([]byte, error)
}

func managedDistribution() distributionStore {
	return distributionStore{root: distributionRoot, link: distributionLink, arch: runtime.GOARCH, retain: 2}
}

func packageVersion(id string) string {
	match := installationID.FindStringSubmatch(id)
	if match == nil {
		return ""
	}
	return match[1]
}

func (s distributionStore) versionDir(id string) string {
	return filepath.Join(s.root, "versions", id)
}

func (s distributionStore) current() (string, error) {
	target, err := os.Readlink(filepath.Join(s.root, "current"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	id := strings.TrimPrefix(target, "versions/")
	if id == target || packageVersion(id) == "" {
		return "", fmt.Errorf("invalid current link %q", target)
	}
	return id, nil
}

func (s distributionStore) state() (installation, error) {
	var state installation
	data, err := os.ReadFile(filepath.Join(s.root, "installation.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	valid := func(id string) bool { return id == "" || packageVersion(id) != "" }
	if !valid(state.Previous) || (state.HoldRelease != "" && !stableDistributionTag.MatchString(state.HoldRelease)) {
		return state, errors.New("invalid committed installation state")
	}
	if p := state.Pending; p != nil {
		if !valid(p.Old) || packageVersion(p.Target) == "" || p.Old == p.Target ||
			(p.Phase != "drain" && p.Phase != "stop" && p.Phase != "activate") {
			return state, errors.New("invalid pending installation state")
		}
	}
	return state, nil
}

func (s distributionStore) save(state installation) error {
	file, err := os.CreateTemp(s.root, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writeErr := json.NewEncoder(file).Encode(state)
	if err := errors.Join(writeErr, file.Chmod(0644), file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(s.root, "installation.json")); err != nil {
		return err
	}
	return syncDirectory(s.root)
}

func (s distributionStore) locked(run func(distributionStore) error) error {
	if s.retain < 2 {
		return errors.New("retain at least two package versions")
	}
	if err := os.MkdirAll(filepath.Join(s.root, "versions"), 0755); err != nil {
		return err
	}
	for _, dir := range []string{s.root, filepath.Join(s.root, "versions")} {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe package directory %s", dir)
		}
		if err := os.Chmod(dir, 0755); err != nil {
			return err
		}
	}
	lock, err := os.OpenFile(filepath.Join(s.root, ".update.lock"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another package or lifecycle operation is running: %w", err)
	}
	if err := lock.Chmod(0644); err != nil {
		return err
	}
	s.lock = lock
	return run(s)
}

func (s distributionStore) switchTo(id string) error {
	link := filepath.Join(s.root, "current")
	if id == "" {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		if err := replaceLink(link, "versions/"+id); err != nil {
			return err
		}
	}
	return syncDirectory(s.root)
}

func replaceLink(link, target string) error {
	if existing, err := os.Readlink(link); err == nil && existing == target {
		return nil
	}
	info, err := os.Lstat(link)
	if err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to replace %s", link)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Dir(link), ".lumiqot-link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	source := filepath.Join(temp, "link")
	if err := os.Symlink(target, source); err != nil {
		return err
	}
	return os.Rename(source, link)
}

func (s distributionStore) invoke(id, action string, seconds int) ([]byte, error) {
	if s.call != nil {
		return s.call(id, action, seconds)
	}
	return s.runCLI(id, action, seconds)
}

func (s distributionStore) action(id, action string, seconds int) error {
	_, err := s.invoke(id, action, seconds)
	if err != nil {
		return fmt.Errorf("%s %s: %w", id, action, err)
	}
	return nil
}

func (s distributionStore) lifecycleStatus(id string) (json.RawMessage, string, error) {
	data, err := s.invoke(id, "status", 8)
	if err != nil {
		return nil, "", err
	}
	var status struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, "", err
	}
	switch status.State {
	case "healthy", "disabled", "draining", "stopped", "failed", "partial":
	default:
		return nil, "", fmt.Errorf("invalid lifecycle state %q", status.State)
	}
	return json.RawMessage(data), status.State, nil
}

func lifecycleReady(phase string) bool {
	return phase == "healthy" || phase == "disabled"
}

func (s distributionStore) start(id string) error {
	if err := s.action(id, "start", 60); err != nil {
		return err
	}
	_, phase, err := s.lifecycleStatus(id)
	if err == nil && !lifecycleReady(phase) {
		err = errors.New("services did not converge to a ready lifecycle state after start")
	}
	return err
}

// Recover either resumes the original active installation or first drains and
// stops the candidate that may have started after the current link switched.
func (s distributionStore) recover(state installation) error {
	p := state.Pending
	current, err := s.current()
	if err != nil {
		return err
	}
	if current != p.Old && current != p.Target {
		return fmt.Errorf("unexpected active installation %q during recovery", current)
	}
	if current == p.Target {
		if err := s.action(p.Target, "drain", 900); err != nil {
			// A failed rollback must not leave the active candidate drained.
			return errors.Join(err, s.start(p.Target))
		}
		if err := s.action(p.Target, "stop", 120); err != nil {
			return errors.Join(err, s.start(p.Target))
		}
		if err := s.switchTo(p.Old); err != nil {
			return err
		}
	}
	if p.Old != "" {
		if err := s.start(p.Old); err != nil {
			return err
		}
	}
	state.Pending = nil
	state.RecoveryError = ""
	return s.save(state)
}

func (s distributionStore) fail(state installation, failure error) error {
	state.LastError = failure.Error()
	recordErr := s.save(state)
	if err := s.recover(state); err != nil {
		state.RecoveryError = err.Error()
		return errors.Join(failure, fmt.Errorf("recovery failed: %w", err), recordErr, s.save(state))
	}
	return errors.Join(failure, recordErr, s.cleanup(state.Pending.Old, state.Previous))
}

func (s distributionStore) recoverPending(state installation) error {
	if err := s.recover(state); err != nil {
		err = fmt.Errorf("recover pending package transaction: %w", err)
		state.RecoveryError = err.Error()
		return errors.Join(err, s.save(state))
	}
	return nil
}

func (s distributionStore) activate(state installation, old, target string, rollback bool) error {
	if old == target {
		return errors.New("target is already current")
	}
	state.Pending = &packageTransaction{Old: old, Target: target, Phase: "drain"}
	if err := s.save(state); err != nil {
		return err
	}
	if old != "" {
		if err := s.action(old, "drain", 900); err != nil {
			return s.fail(state, err)
		}
		state.Pending.Phase = "stop"
		if err := s.save(state); err != nil {
			return s.fail(state, err)
		}
		if err := s.action(old, "stop", 120); err != nil {
			return s.fail(state, err)
		}
	}
	state.Pending.Phase = "activate"
	if err := s.save(state); err != nil {
		return s.fail(state, err)
	}
	if err := s.switchTo(target); err != nil {
		return s.fail(state, err)
	}
	if err := s.start(target); err != nil {
		return s.fail(state, err)
	}
	if err := s.ensureLink(); err != nil {
		return s.fail(state, err)
	}

	committed := state
	committed.Pending = nil
	committed.LastError = ""
	committed.RecoveryError = ""
	committed.HoldRelease = ""
	if rollback {
		committed.HoldRelease = packageVersion(old)
	}
	if old != "" && verifyDistribution(s.versionDir(old), packageVersion(old), s.arch, false) == nil {
		committed.Previous = old
	} else if committed.Previous == target {
		committed.Previous = ""
	}
	// If publication of the state failed, leave the new installation intact.
	// A subsequent reconciliation uses whichever journal record reached disk.
	if err := s.save(committed); err != nil {
		return err
	}
	return s.cleanup(target, committed.Previous)
}

func (s distributionStore) ensureLink() error {
	if err := os.MkdirAll(filepath.Dir(s.link), 0755); err != nil {
		return err
	}
	if err := replaceLink(s.link, filepath.Join(s.root, "current", packageCLI)); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(s.link))
}

func (s distributionStore) linkReady() bool {
	target, err := os.Readlink(s.link)
	return err == nil && target == filepath.Join(s.root, "current", packageCLI)
}

func (s distributionStore) cleanup(current, previous string) error {
	entries, err := os.ReadDir(filepath.Join(s.root, "versions"))
	if err != nil {
		return err
	}
	type candidate struct {
		id      string
		created time.Time
	}
	var older []candidate
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".stage-") {
			if err := os.RemoveAll(s.versionDir(name)); err != nil {
				return err
			}
			continue
		}
		if !entry.IsDir() || packageVersion(name) == "" || name == current || name == previous {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		older = append(older, candidate{name, info.ModTime()})
	}
	sort.Slice(older, func(i, j int) bool { return older[i].created.After(older[j].created) })
	remaining := s.retain
	if current != "" {
		remaining--
	}
	if previous != "" {
		remaining--
	}
	for i, entry := range older {
		if i < remaining {
			continue
		}
		if err := os.RemoveAll(s.versionDir(entry.id)); err != nil {
			return err
		}
	}
	return syncDirectory(filepath.Join(s.root, "versions"))
}

func (s distributionStore) stage(release distributionRelease) (string, error) {
	return s.stagePackage(release.tag, release.stage)
}

func (s distributionStore) stageLocal(tag string, source io.Reader, digest string) (string, error) {
	release := distributionRelease{tag: tag, arch: s.arch}
	return s.stagePackage(tag, func(root string) error {
		return release.stageArchive(root, source, digest)
	})
}

func (s distributionStore) stagePackage(tag string, unpack func(string) error) (string, error) {
	stage, err := os.MkdirTemp(filepath.Join(s.root, "versions"), ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := unpack(stage); err != nil {
		return "", err
	}
	if err := os.Chmod(stage, 0755); err != nil {
		return "", err
	}
	if err := syncDirectory(stage); err != nil {
		return "", err
	}
	id := tag + "-" + strings.TrimPrefix(filepath.Base(stage), ".stage-")
	destination := s.versionDir(id)
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("staged destination unavailable: %s", destination)
	}
	if err := os.Rename(stage, destination); err != nil {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return "", err
	}
	if err := verifyDistribution(destination, tag, s.arch, false); err != nil {
		return "", errors.Join(err, os.RemoveAll(destination))
	}
	out, err := s.invoke(id, "version", 8)
	if err != nil || strings.TrimSpace(string(out)) != tag {
		return "", errors.Join(fmt.Errorf("staged CLI did not report package version %s", tag), err, os.RemoveAll(destination))
	}
	return id, nil
}

func userManager() (*user.User, error) { return user.Lookup(distributionUser) }

func prepareUserManager() error {
	if os.Geteuid() != 0 {
		return errors.New("package installation requires root for system integration")
	}
	account, err := userManager()
	if err != nil {
		return err
	}
	for _, command := range [][]string{
		{"/usr/bin/loginctl", "enable-linger", account.Username},
		{"/usr/bin/systemctl", "start", "user@" + account.Uid + ".service"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", command[0], err, out)
		}
	}
	return nil
}

func (s distributionStore) runCLI(id, action string, seconds int) ([]byte, error) {
	if err := verifyDistribution(s.versionDir(id), packageVersion(id), s.arch, true); err != nil {
		return nil, err
	}
	account, err := userManager()
	if err != nil {
		return nil, err
	}
	runtimeDir := "/run/user/" + account.Uid
	args := []string{"-i", "HOME=" + account.HomeDir, "USER=" + account.Username,
		"PATH=/usr/local/bin:/usr/bin:/bin", "XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + runtimeDir + "/bus"}
	if action != "status" && action != "version" && s.lock != nil {
		args = append(args, "LUMIQOT_MANAGER_TRANSACTION=1")
	}
	args = append(args, filepath.Join(s.versionDir(id), packageCLI))
	if action == "version" {
		args = append(args, "version")
	} else {
		args = append(args, "lifecycle", action, "--timeout", strconv.Itoa(seconds))
		if action == "status" {
			args = append(args, "--json")
		}
	}
	command := "/usr/bin/env"
	if os.Geteuid() == 0 {
		command = "/usr/sbin/runuser"
		args = append([]string{"-u", account.Username, "--", "/usr/bin/env"}, args...)
	} else if strconv.Itoa(os.Geteuid()) != account.Uid {
		return nil, fmt.Errorf("lifecycle reports require root or %s", account.Username)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+5)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = "/"
	if s.lock != nil {
		// Hold the transaction lock through an orphaned lifecycle call if
		// Manager exits before its child does.
		cmd.ExtraFiles = []*os.File{s.lock}
	}
	out, err := cmd.Output()
	var processError *exec.ExitError
	if errors.As(err, &processError) {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(processError.Stderr)))
	}
	return out, err
}
