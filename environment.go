package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const environmentRoot = "/opt/lumiqo/environment"
const environmentBin = environmentRoot + "/current/bin"
const analysisSmoke = "import bs4, duckdb, matplotlib, numpy, openpyxl, pandas, polars, pyarrow, requests, scipy"

//go:embed install-environment.sh
var environmentInstaller string

//go:embed analysis/pyproject.toml
var analysisProject string

//go:embed analysis/uv.lock
var analysisLock string

// The component history lock serializes provisioning and publication.
func environmentRevision() string {
	revision := sha256.Sum256([]byte(environmentInstaller + analysisProject + analysisLock + analysisSmoke + strings.Join(packages, "\n")))
	return fmt.Sprintf("%x", revision[:12])
}

func managedEnvironmentReady() bool {
	target, err := os.Readlink(environmentRoot + "/current")
	if err != nil || target != "versions/"+environmentRevision() {
		return false
	}
	if _, err := os.Stat(environmentRoot + "/current/.complete"); err != nil {
		return false
	}
	for _, name := range []string{"fd", "bat"} {
		info, err := os.Stat(filepath.Join(environmentBin, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return false
		}
	}
	for _, name := range []string{"uv", "mise", "duckdb", "node", "npm", "npx", "python-analysis", "fd", "bat"} {
		if _, err := os.Lstat(filepath.Join("/usr/local/bin", name)); err != nil {
			return false
		}
	}
	for _, path := range []string{"/home/lumiqo/docs/ENVIRONMENT.md", "/home/lumiqo/.venvs/analysis"} {
		if _, err := os.Lstat(path); err != nil {
			return false
		}
	}
	return environmentHealth()["ready"].(bool)
}

func provisionEnvironment(stderr io.Writer) error {
	inputs, err := os.MkdirTemp("", "lumiqo-environment-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(inputs)
	for name, contents := range map[string]string{"pyproject.toml": analysisProject, "uv.lock": analysisLock} {
		if err := os.WriteFile(filepath.Join(inputs, name), []byte(contents), 0600); err != nil {
			return err
		}
	}
	cmd := exec.Command("/bin/bash", "-s", "--", environmentRoot, inputs, environmentRevision(), runtime.GOARCH, analysisSmoke)
	cmd.Args = append(cmd.Args, packages...)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	cmd.Stdin = strings.NewReader(environmentInstaller)
	cmd.Stdout, cmd.Stderr = stderr, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("provision CLI and analysis environment: %w", err)
	}
	return nil
}

func environmentHealth() map[string]any {
	versions := map[string]string{}
	scratch, err := os.MkdirTemp("", "lumiqo-health-")
	if err != nil {
		return map[string]any{"ready": false, "versions": versions}
	}
	defer os.RemoveAll(scratch)
	check := func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(environmentBin, name), args...)
		cmd.Dir = scratch
		cmd.Env = []string{"HOME=" + scratch, "XDG_CACHE_HOME=" + scratch, "MPLCONFIGDIR=" + scratch,
			"PATH=" + environmentBin + ":/usr/bin:/bin", "LANG=C.UTF-8"}
		return cmd.Output()
	}
	ready := true
	for _, name := range []string{"uv", "mise", "node", "npm", "npx", "duckdb", "python-analysis"} {
		output, err := check(name, "--version")
		if err != nil {
			ready = false
			continue
		}
		versions[name] = strings.TrimSpace(string(output))
	}
	if ready {
		_, err := check("python-analysis", "-I", "-B", "-c", analysisSmoke)
		ready = err == nil
	}
	return map[string]any{"ready": ready, "versions": versions}
}
