package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLatestWorkerRelease(t *testing.T) {
	for _, tc := range []struct {
		tag, current, want string
		prerelease         bool
	}{
		{"v1.10.0", "", "v1.10.0", false},
		{"v1.10.0", "v1.9.0", "v1.10.0", false},
		{"v1.9.0", "v1.10.0", "v1.10.0", false},
		{"v1.10.0-rc.1", "", "", true},
		{"../v1.10.0", "", "", false},
	} {
		t.Run(tc.tag+"/"+tc.current, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"tag_name":%q,"prerelease":%t}`, tc.tag, tc.prerelease)
			}))
			defer server.Close()
			release, err := latestWorkerRelease(server.Client(), server.URL, server.URL, "amd64", tc.current)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted invalid release")
				}
			} else if err != nil || release.tag != tc.want {
				t.Fatalf("release tag = %q, %v, want %q", release.tag, err, tc.want)
			}
		})
	}
}

func TestStageWorkerRelease(t *testing.T) {
	for _, tc := range []struct {
		version, wantError string
		wrongHash          bool
	}{
		{"v1.10.0", "", false},
		{"v1.10.0", "SHA-256 mismatch", true},
		{"v1.9.0", "does not report release", false},
	} {
		t.Run(fmt.Sprintf("%s/%t", tc.version, tc.wrongHash), func(t *testing.T) {
			r := workerRelease{tag: "v1.10.0", arch: "amd64"}
			archive := testWorkerArchive(t, r.archiveName(), tc.version)
			name := r.archiveName() + ".tar.gz"
			digest := sha256.Sum256(archive)
			if tc.wrongHash {
				digest[0]++
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/v1.10.0/lumiqot-linux-v1.10.0-SHA256SUMS":
					fmt.Fprintf(w, "%x  %s\n", digest, name)
				case "/v1.10.0/" + name:
					_, _ = w.Write(archive)
				default:
					http.NotFound(w, req)
				}
			}))
			defer server.Close()
			r.assets, r.client = server.URL, server.Client()

			dir := t.TempDir()
			err := stageWorker(dir, r)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("stage returned %v, want %q", err, tc.wantError)
				}
			} else if err != nil || !workerPayloadReady(filepath.Join(dir, "payload")) {
				t.Fatalf("staged Worker not ready: %v", err)
			}
		})
	}
}

func testWorkerArchive(t *testing.T, root, version string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	writer := tar.NewWriter(gz)
	for _, dir := range []string{"bin", "libexec", "libexec/lumiqot", "licenses"} {
		if err := writer.WriteHeader(&tar.Header{Name: root + "/" + dir, Mode: 0755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		"bin/lumiqot":                   "#!/bin/sh\nprintf '%s\\n' " + version + "\n",
		"libexec/lumiqot/lumiqo-worker": "#!/bin/sh\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: root + "/" + path, Mode: 0755, Size: int64(len(text))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestWorkerServiceUpdatePreservesStoppedState(t *testing.T) {
	for _, tc := range []struct {
		configure, running bool
		want               []string
	}{
		{false, false, nil},
		{false, true, []string{"try-restart lumiqo-worker.service"}},
		{true, false, []string{"daemon-reload", "enable --now lumiqo-worker.service"}},
	} {
		var calls []string
		err := applyWorkerService(tc.configure, tc.running, func(args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return nil
		})
		if err != nil || !reflect.DeepEqual(calls, tc.want) {
			t.Errorf("systemctl calls = %v, %v, want %v", calls, err, tc.want)
		}
	}
}

func TestWorkerInstallCLIRejectsUnsupportedRequests(t *testing.T) {
	for _, args := range [][]string{
		{"install"}, {"install", "other-repo"}, {"install", workerRepo, "extra"},
		{"setup", "--apply", "--worker"}, {"reconcile", "--apply", "--worker"},
	} {
		if err := run(args, nil, io.Discard, io.Discard); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	if err := run([]string{"install", "--help"}, os.Stdin, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerUpgradePublishesAndRetriesFailedRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	for _, tag := range []string{"v1.9.0", "v1.10.0"} {
		for _, path := range []string{"bin/lumiqot", "libexec/lumiqot/lumiqo-worker"} {
			binary := filepath.Join(root, "versions", tag, path)
			if err := os.MkdirAll(filepath.Dir(binary), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	const previous = "versions/v1.9.0"
	link := filepath.Join(root, "current")
	if err := os.Symlink(previous, link); err != nil {
		t.Fatal(err)
	}
	commands := t.TempDir()
	t.Setenv("PATH", commands)
	log := filepath.Join(commands, "calls")
	t.Setenv("WORKER_TEST_CALLS", log)
	t.Setenv("WORKER_TEST_LINK", link)
	script := `#!/bin/sh
printf '%s\n' "$1" >> "$WORKER_TEST_CALLS"
case "$1" in
    try-restart)
        test "$(/usr/bin/readlink "$WORKER_TEST_LINK")" = versions/v1.10.0 || exit 10
        test "$WORKER_TEST_FAIL" != yes
        ;;
    start) test "$(/usr/bin/readlink "$WORKER_TEST_LINK")" = versions/v1.9.0 ;;
    *) exit 11 ;;
esac
`
	if err := os.WriteFile(filepath.Join(commands, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	release := workerRelease{tag: "v1.10.0"}
	state := service{Active: "active"}
	t.Setenv("WORKER_TEST_FAIL", "yes")
	if err := installWorkerService(root, release, previous, false, state, nil, io.Discard); err == nil {
		t.Fatal("expected restart failure")
	}
	if got, err := os.Readlink(link); err != nil || got != previous {
		t.Fatalf("link after failed restart = %q, %v", got, err)
	}
	t.Setenv("WORKER_TEST_FAIL", "no")
	if err := installWorkerService(root, release, previous, false, state, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != "versions/v1.10.0" {
		t.Fatalf("link after retry = %q, %v", got, err)
	}
	if calls, err := os.ReadFile(log); err != nil || string(calls) != "try-restart\nstart\ntry-restart\n" {
		t.Fatalf("systemctl calls = %q, %v", calls, err)
	}
}
