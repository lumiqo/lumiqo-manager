package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

const workerTag = "v0.1.0"
const workerRoot = "/opt/lumiqo/worker"
const workerConfig = "/home/lumiqo/.config/lumiqo/worker.json"
const workerUnit = "lumiqo-worker.service"

var workerHashes = map[string]string{
	"amd64": "f2a2baa6ecc764343d47fd1abaee0d4d06ac1fd97bfedb10eb5df715c3ef5a84",
	"arm64": "874d84779258f599c9681aee92e1b9c1df8679ec1f6dc25acd815a26bf794d20",
}

const unitContents = `[Unit]
Description=Lumiqo Worker
Wants=network-online.target
After=network-online.target
ConditionPathExists=/home/lumiqo/.config/lumiqo/worker.json

[Service]
User=lumiqo
WorkingDirectory=/home/lumiqo
StateDirectory=lumiqo-worker
Environment=PATH=/home/lumiqo/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/lumiqo/worker/current/libexec/lumiqot/lumiqo-worker -config /home/lumiqo/.config/lumiqo/worker.json -mode poll
Restart=on-failure
RestartSec=10s

[Install]
WantedBy=multi-user.target
`

func provisionWorker(stdin io.Reader, stderr io.Writer) (any, error) {
	if os.Geteuid() != 0 || os.Getenv("WSL_DISTRO_NAME") != "Lumiqo" {
		return nil, errors.New("Worker provisioning requires root in the Lumiqo WSL distribution")
	}
	if err := exec.Command("id", "-u", "lumiqo").Run(); err != nil {
		return nil, fmt.Errorf("Lumiqo user is missing: %w", err)
	}
	if err := os.MkdirAll(workerRoot+"/versions", 0755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(workerRoot+"/.setup.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	if _, err := setup(true, stdin, stderr, "bubblewrap", "gh"); err != nil {
		return nil, err
	}
	target := "versions/" + workerTag
	temp, err := os.MkdirTemp(workerRoot, ".setup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(temp)
	if _, err := os.Stat(filepath.Join(workerRoot, target)); errors.Is(err, os.ErrNotExist) {
		if err := stageWorker(temp); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(temp, "payload"), filepath.Join(workerRoot, target)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	for _, binary := range []string{"bin/lumiqot", "libexec/lumiqot/lumiqo-worker"} {
		info, err := os.Stat(filepath.Join(workerRoot, target, binary))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return nil, fmt.Errorf("Worker version directory contains no executable %s", binary)
		}
	}
	if err := os.Symlink(target, filepath.Join(temp, "current")); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(temp, "current"), workerRoot+"/current"); err != nil {
		return nil, err
	}
	unitPath := "/etc/systemd/system/" + workerUnit
	unitFile, err := os.CreateTemp(filepath.Dir(unitPath), ".lumiqo-worker-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(unitFile.Name())
	writeErr := os.WriteFile(unitFile.Name(), []byte(unitContents), 0644)
	closeErr := unitFile.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	if err := os.Chmod(unitFile.Name(), 0644); err != nil {
		return nil, err
	}
	if err := os.Rename(unitFile.Name(), unitPath); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", workerUnit}} {
		if err := interactive(stdin, stderr, stderr, "systemctl", args...); err != nil {
			return nil, fmt.Errorf("systemctl %s: %w", args[0], err)
		}
	}
	return map[string]any{"worker": worker(workerUnit, false), "worker_configured": workerConfigured()}, nil
}

func workerConfigured() bool {
	info, err := os.Stat(workerConfig)
	return err == nil && info.Mode().IsRegular()
}

func stageWorker(temp string) error {
	digest, ok := workerHashes[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("unsupported Worker architecture %s", runtime.GOARCH)
	}
	name := "lumiqot-linux-" + workerTag + "-linux-" + runtime.GOARCH
	url := "https://github.com/lumiqo/lumiqot-linux/releases/download/" + workerTag + "/" + name + ".tar.gz"
	client := &http.Client{Timeout: 2 * time.Minute}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download Worker: %s", response.Status)
	}
	archive := filepath.Join(temp, "worker.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, 32<<20))
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Worker archive SHA-256 mismatch")
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
	return nil
}
