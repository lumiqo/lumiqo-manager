package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const distributionLatestRelease = "https://github.com/lumiqo/lumiqot-linux/releases/latest"
const distributionReleaseAssets = "https://github.com/lumiqo/lumiqot-linux/releases/download"

var stableDistributionTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type distributionRelease struct {
	tag, arch, assets string
	client            *http.Client
}

func latestDistributionRelease(client *http.Client, endpoint, assets, arch string) (distributionRelease, error) {
	if arch != "amd64" && arch != "arm64" {
		return distributionRelease{}, fmt.Errorf("unsupported Distribution architecture %s", arch)
	}
	redirectClient := *client
	redirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	response, err := redirectClient.Get(endpoint)
	if err != nil {
		return distributionRelease{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return distributionRelease{}, fmt.Errorf("fetch latest Distribution release from %s: expected redirect, got %s", endpoint, response.Status)
	}
	target, err := response.Location()
	if err != nil {
		return distributionRelease{}, fmt.Errorf("read Distribution release redirect: %w", err)
	}
	source := response.Request.URL
	prefix := strings.TrimSuffix(source.Path, "/latest") + "/tag/"
	tag, found := strings.CutPrefix(target.Path, prefix)
	if target.Scheme != source.Scheme || target.Host != source.Host || target.User != nil ||
		!found || !stableDistributionTag.MatchString(tag) {
		return distributionRelease{}, errors.New("unexpected Distribution release redirect")
	}
	return distributionRelease{tag: tag, arch: arch, assets: assets, client: client}, nil
}

func distributionVersionNewer(candidate, current string) bool {
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

func (r distributionRelease) archiveName() string {
	return "lumiqot-linux-" + r.tag + "-linux-" + r.arch
}

func (r distributionRelease) openAsset(name string) (io.ReadCloser, error) {
	return openDistributionDownload(r.client, r.assets+"/"+r.tag+"/"+name)
}

func (r distributionRelease) checksum() (string, error) {
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
		return "", errors.New("Distribution SHA256SUMS exceeds size limit")
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
			return "", fmt.Errorf("invalid Distribution checksum for %s", name)
		}
		digest = strings.ToLower(fields[0])
	}
	if digest == "" {
		return "", fmt.Errorf("Distribution SHA256SUMS is missing %s", name)
	}
	return digest, nil
}

func openDistributionDownload(client *http.Client, url string) (io.ReadCloser, error) {
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("fetch Distribution release from %s: %s", url, response.Status)
	}
	return response.Body, nil
}
