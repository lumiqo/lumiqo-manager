package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	googleCloudCLIPackage = "google-cloud-cli"
	googleCloudCLIKeyring = "/usr/share/keyrings/cloud.google.gpg"
	googleCloudCLISource  = "/etc/apt/sources.list.d/google-cloud-sdk.list"
	googleCloudCLISuite   = "cloud-sdk"
	googleCloudCLIRepo    = "https://packages.cloud.google.com/apt"
	googleCloudCLILine    = "deb [signed-by=" + googleCloudCLIKeyring + "] " + googleCloudCLIRepo + " " + googleCloudCLISuite + " main"
)

func googleCloudCLIConfigured() bool {
	source, err := os.ReadFile(googleCloudCLISource)
	if err != nil || strings.TrimSpace(string(source)) != googleCloudCLILine {
		return false
	}
	key, err := os.Stat(googleCloudCLIKeyring)
	if err != nil || !key.Mode().IsRegular() || key.Size() == 0 || key.Mode().Perm()&0004 == 0 {
		return false
	}
	binary, err := os.Stat("/usr/bin/gcloud")
	return err == nil && binary.Mode().IsRegular() && binary.Mode().Perm()&0111 != 0
}

func googleCloudCLIReady() bool {
	return googleCloudCLIConfigured() && len(missingAptPackages([]string{googleCloudCLIPackage})) == 0
}

func installGoogleCloudCLI(stdin io.Reader, stderr io.Writer) error {
	const script = `
set -euo pipefail
keyring=$1
source=$2
line=$3
key_url=$4
package=$5
umask 022
install -d -m 0755 "$(dirname "$keyring")" "$(dirname "$source")"
key_tmp=
source_tmp=
trap 'rm -f -- "${key_tmp:-}" "${source_tmp:-}"' EXIT
key_tmp=$(mktemp "${keyring}.XXXXXX")
source_tmp=$(mktemp "${source}.XXXXXX")
curl --fail --silent --show-error --location --retry 2 --max-time 30 \
    --proto '=https' --proto-redir '=https' "$key_url" |
    gpg --no-options --batch --dearmor > "$key_tmp"
printf '%s\n' "$line" > "$source_tmp"
chmod 0644 "$key_tmp" "$source_tmp"
mv -fT "$key_tmp" "$keyring"
mv -fT "$source_tmp" "$source"
apt-get update -o APT::Update::Error-Mode=any
apt-get install -y -- "$package"
`
	command := "/bin/bash"
	args := []string{"-c", script, "bash", googleCloudCLIKeyring, googleCloudCLISource, googleCloudCLILine,
		googleCloudCLIRepo + "/doc/apt-key.gpg", googleCloudCLIPackage + "/" + googleCloudCLISuite}
	if os.Geteuid() != 0 {
		args = append([]string{command}, args...)
		command = "sudo"
	}
	if err := interactive(stdin, stderr, stderr, command, args...); err != nil {
		return fmt.Errorf("install Google Cloud CLI from packages.cloud.google.com: %w", err)
	}
	if !googleCloudCLIReady() {
		return fmt.Errorf("Google Cloud CLI installation is incomplete")
	}
	return nil
}
