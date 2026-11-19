package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const workerRepo = "lumiqo-worker"
const workerRoot = "/opt/lumiqo/worker"
const workerConfig = "/home/lumiqo/.config/lumiqo/worker.json"
const workerUnit = "lumiqo-worker.service"

const unitContents = `[Unit]
Description=Lumiqo Worker
Wants=network-online.target
After=network-online.target
ConditionPathExists=/home/lumiqo/.config/lumiqo/worker.json

[Service]
User=lumiqo
WorkingDirectory=/home/lumiqo
StateDirectory=lumiqo-worker
Environment=PATH=` + environmentBin + `:/home/lumiqo/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/lumiqo/worker/current/libexec/lumiqot/lumiqo-worker -config /home/lumiqo/.config/lumiqo/worker.json -mode poll
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
`

func validateWorkerHost(installed bool) error {
	_, managed := os.Stat("/etc/lumiqo/winapp-managed")
	if os.Geteuid() != 0 || (os.Getenv("WSL_DISTRO_NAME") != "Lumiqo" && !(host().WSL && (managed == nil || installed))) {
		return errors.New("Worker provisioning requires root in the Lumiqo WSL distribution")
	}
	if err := exec.Command("id", "-u", "lumiqo").Run(); err != nil {
		return fmt.Errorf("Lumiqo user is missing: %w", err)
	}
	return nil
}

func installWorkerService(root string, release workerRelease, current string, configure bool, state service, stdin io.Reader, stderr io.Writer) error {
	for _, dir := range []string{filepath.Dir(root), root, root + "/versions"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0755); err != nil {
			return err
		}
	}
	temp, err := os.MkdirTemp(root, ".setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)

	target := "versions/" + release.tag
	destination := filepath.Join(root, target)
	repairPayload := !workerPayloadReady(destination)
	if repairPayload {
		if err := stageWorker(temp, release); err != nil {
			return err
		}
		previous := filepath.Join(temp, "previous")
		if err := os.Rename(destination, previous); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(filepath.Join(temp, "payload"), destination); err != nil {
			if restoreErr := os.Rename(previous, destination); restoreErr != nil && !errors.Is(restoreErr, os.ErrNotExist) {
				return errors.Join(err, restoreErr)
			}
			return err
		}
	}
	if configure && state.Load == "not-found" {
		if err := installWorkerUnit("/etc/systemd/system/" + workerUnit); err != nil {
			return err
		}
	}

	changed := current != target
	link := root + "/current"
	publish := func(version string) error {
		pending := filepath.Join(temp, "current")
		if err := os.Symlink(version, pending); err != nil {
			return err
		}
		return os.Rename(pending, link)
	}
	if changed {
		if err := publish(target); err != nil {
			return err
		}
	}

	systemctl := func(args ...string) error {
		if err := interactive(stdin, stderr, stderr, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", args[0], err)
		}
		return nil
	}
	err = applyWorkerService(configure, state.Active == "active" && (changed || repairPayload || workerNeedsRestart(state, release.tag)), systemctl)
	if err == nil || !changed {
		return err
	}
	if current == "" {
		return errors.Join(err, os.Remove(link))
	}
	if rollbackErr := publish(current); rollbackErr != nil {
		return errors.Join(err, rollbackErr)
	}
	if state.Active == "active" {
		return errors.Join(err, systemctl("start", workerUnit))
	}
	return err
}

// A previous update may have published current before it could restart the service.
func workerNeedsRestart(state service, tag string) bool {
	if state.Active != "active" || state.MainPID <= 0 {
		return false
	}
	running, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", state.MainPID))
	return err == nil && strings.HasPrefix(running, workerRoot+"/versions/") &&
		running != filepath.Join(workerRoot, "versions", tag, "libexec/lumiqot/lumiqo-worker")
}

func applyWorkerService(configure, restart bool, systemctl func(...string) error) error {
	if configure {
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		if err := systemctl("enable", "--now", workerUnit); err != nil {
			return err
		}
	}
	if restart {
		return systemctl("try-restart", workerUnit)
	}
	return nil
}

func workerPayloadReady(root string) bool {
	for _, binary := range []string{"bin/lumiqot", "libexec/lumiqot/lumiqo-worker"} {
		info, err := os.Stat(filepath.Join(root, binary))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return false
		}
	}
	return true
}

func installWorkerUnit(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".lumiqo-worker-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := io.WriteString(file, unitContents)
	if err := errors.Join(writeErr, file.Chmod(0644), file.Close()); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func workerConfigured() bool {
	info, err := os.Stat(workerConfig)
	return err == nil && info.Mode().IsRegular()
}

func stageWorker(temp string, release workerRelease) error {
	digest, err := release.checksum()
	if err != nil {
		return err
	}
	name := release.archiveName()
	response, err := release.openAsset(name + ".tar.gz")
	if err != nil {
		return err
	}
	defer response.Close()
	archive := filepath.Join(temp, "worker.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	const maxArchive = 32 << 20
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response, maxArchive+1))
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if size > maxArchive || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Worker archive size or SHA-256 mismatch")
	}
	payload := filepath.Join(temp, "payload")
	if err := os.Mkdir(payload, 0755); err != nil {
		return err
	}
	cmd := exec.Command("tar", "-xzf", archive, "-C", payload, "--strip-components=1",
		"--no-same-owner", "--no-same-permissions", name+"/bin", name+"/libexec", name+"/licenses")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract Worker: %w: %s", err, output)
	}
	if err := makeWorkerReadable(payload); err != nil {
		return err
	}
	output, err := probe(filepath.Join(payload, "bin/lumiqot"), "version")
	if err != nil || string(output) != release.tag+"\n" || !workerPayloadReady(payload) {
		return fmt.Errorf("Worker payload does not report release %s or is incomplete", release.tag)
	}
	return nil
}

func makeWorkerReadable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected entry in Worker payload: %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0644)
		if entry.IsDir() || relative == "bin/lumiqot" || relative == "libexec/lumiqot/lumiqo-worker" {
			mode = 0755
		}
		return os.Chmod(path, mode)
	})
}
