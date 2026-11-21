package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

const (
	githubCLIKeyring = "/etc/apt/keyrings/githubcli-archive-keyring.gpg"
	githubCLISource  = "/etc/apt/sources.list.d/github-cli.list"
	githubCLIKeyHash = "6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b"
)

func githubCLISourceLine() string {
	return fmt.Sprintf("deb [arch=%s signed-by=%s] https://cli.github.com/packages stable main",
		runtime.GOARCH, githubCLIKeyring)
}

func githubCLIReady() bool {
	source, err := os.ReadFile(githubCLISource)
	if err != nil || strings.TrimSpace(string(source)) != githubCLISourceLine() {
		return false
	}
	key, err := os.ReadFile(githubCLIKeyring)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(key)) != githubCLIKeyHash {
		return false
	}
	// Check the apt-managed binary. A per-user override does not satisfy setup.
	help, err := probe("/usr/bin/gh", "api", "--help")
	return err == nil && strings.Contains(string(help), "--slurp")
}

func installGitHubCLI(stdin io.Reader, stderr io.Writer) error {
	const script = `
set -eu
keyring=$1
source=$2
expected=$3
line=$4
install -d -m 0755 "$(dirname "$keyring")" "$(dirname "$source")"
key_tmp=$(mktemp "${keyring}.XXXXXX")
source_tmp=$(mktemp "${source}.XXXXXX")
trap 'rm -f -- "$key_tmp" "$source_tmp"' EXIT
if [ ! -f "$keyring" ] || ! printf '%s  %s\n' "$expected" "$keyring" | sha256sum --check --status; then
    curl --fail --silent --show-error --location --max-time 30 \
        --proto '=https' --proto-redir '=https' \
        https://cli.github.com/packages/githubcli-archive-keyring.gpg -o "$key_tmp"
    printf '%s  %s\n' "$expected" "$key_tmp" | sha256sum --check
    chmod 0644 "$key_tmp"
    mv -fT "$key_tmp" "$keyring"
fi
printf '%s\n' "$line" > "$source_tmp"
if ! cmp -s "$source_tmp" "$source"; then
    chmod 0644 "$source_tmp"
    mv -fT "$source_tmp" "$source"
fi
apt-get update -o APT::Update::Error-Mode=any
apt-get install -y -- gh/stable
`
	command := "/bin/sh"
	args := []string{"-c", script, "sh", githubCLIKeyring, githubCLISource, githubCLIKeyHash, githubCLISourceLine()}
	if os.Geteuid() != 0 {
		args = append([]string{command}, args...)
		command = "sudo"
	}
	if err := interactive(stdin, stderr, stderr, command, args...); err != nil {
		return fmt.Errorf("install GitHub CLI from cli.github.com: %w", err)
	}
	if !githubCLIReady() {
		return fmt.Errorf("GitHub CLI installation does not support gh api --slurp")
	}
	return nil
}
