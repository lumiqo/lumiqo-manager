package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const workerReleaseAPI = "https://api.github.com/repos/lumiqo/lumiqot-linux/releases/latest"
const workerReleaseAssets = "https://github.com/lumiqo/lumiqot-linux/releases/download"

var stableWorkerTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type workerRelease struct {
	tag, arch, assets string
	client            *http.Client
}

func inspectWorker(selected bool, stdin io.Reader, stderr io.Writer) (component, error) {
	c := component{name: "worker", policy: "optional", selected: selected}
	current, err := os.Readlink(filepath.Join(workerRoot, "current"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("read Worker current link: %w", err)
	}
	if current != "" {
		tag := filepath.Base(current)
		if !stableWorkerTag.MatchString(tag) || current != "versions/"+tag {
			return c, fmt.Errorf("unexpected Worker current target %q", current)
		}
		c.currentVersion = tag
		c.present = true
	}
	if !selected && !c.present {
		return c, nil
	}

	if selected {
		if err := validateWorkerHost(c.present); err != nil {
			return c, err
		}
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	release, err := latestWorkerRelease(client, workerReleaseAPI, workerReleaseAssets, runtime.GOARCH, c.currentVersion)
	if err != nil {
		return c, err
	}
	c.targetVersion = release.tag
	c.missingPackages = missingAptPackages([]string{"bubblewrap"})
	state := worker(workerUnit, false)
	configure := selected && (!c.present || state.Load == "not-found" || state.UnitFileState == "disabled")
	c.ready = c.currentVersion == c.targetVersion && workerPayloadReady(workerRoot+"/current") &&
		len(c.missingPackages) == 0 && !configure && !workerNeedsRestart(state, release.tag)
	c.install = func() error {
		if err := validateWorkerHost(c.present); err != nil {
			return err
		}
		if state.Error != "" || state.Load == "unknown" {
			return fmt.Errorf("cannot determine Worker service state: %s", state.Error)
		}
		if len(c.missingPackages) != 0 {
			if _, err := setup(true, stdin, stderr, "bubblewrap"); err != nil {
				return err
			}
		}
		return installWorkerService(workerRoot, release, current, configure, state, stdin, stderr)
	}
	return c, nil
}

func latestWorkerRelease(client *http.Client, endpoint, assets, arch, current string) (workerRelease, error) {
	if arch != "amd64" && arch != "arm64" {
		return workerRelease{}, fmt.Errorf("unsupported Worker architecture %s", arch)
	}
	body, err := openWorkerDownload(client, endpoint)
	if err != nil {
		return workerRelease{}, err
	}
	defer body.Close()
	var metadata struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&metadata); err != nil {
		return workerRelease{}, fmt.Errorf("decode Worker release: %w", err)
	}
	if metadata.Draft || metadata.Prerelease || !stableWorkerTag.MatchString(metadata.Tag) {
		return workerRelease{}, fmt.Errorf("unexpected stable Worker release %q", metadata.Tag)
	}
	tag := metadata.Tag
	if current != "" && workerVersionNewer(current, tag) {
		tag = current
	}
	return workerRelease{tag: tag, arch: arch, assets: assets, client: client}, nil
}

func workerVersionNewer(candidate, current string) bool {
	a, b := strings.Split(candidate[1:], "."), strings.Split(current[1:], ".")
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func (r workerRelease) archiveName() string {
	return "lumiqot-linux-" + r.tag + "-linux-" + r.arch
}

func (r workerRelease) openAsset(name string) (io.ReadCloser, error) {
	return openWorkerDownload(r.client, r.assets+"/"+r.tag+"/"+name)
}

func (r workerRelease) checksum() (string, error) {
	body, err := r.openAsset("lumiqot-linux-" + r.tag + "-SHA256SUMS")
	if err != nil {
		return "", err
	}
	defer body.Close()
	const maxChecksums = 64 << 10
	data, err := io.ReadAll(io.LimitReader(body, maxChecksums+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxChecksums {
		return "", errors.New("Worker SHA256SUMS exceeds size limit")
	}
	name := r.archiveName() + ".tar.gz"
	var digest string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil || len(decoded) != 32 || digest != "" {
			return "", fmt.Errorf("invalid Worker checksum for %s", name)
		}
		digest = strings.ToLower(fields[0])
	}
	if digest == "" {
		return "", fmt.Errorf("Worker SHA256SUMS is missing %s", name)
	}
	return digest, nil
}

func openWorkerDownload(client *http.Client, url string) (io.ReadCloser, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("fetch Worker release from %s: %s", url, response.Status)
	}
	return response.Body, nil
}
