package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

type archiveEntry struct {
	name, data string
	kind       byte
	mode       int64
}

var validEntries = []archiveEntry{
	{packageCLI, "test CLI", tar.TypeReg, 0755},
	{"resources/example.txt", "static resource", tar.TypeReg, 0644},
}

func testManifest(tag string) packageManifest {
	m := packageManifest{1, "lumiqot-linux", tag, "linux", "amd64", packageCLI, lifecycleProtocol, nil}
	for _, entry := range validEntries {
		m.Files = append(m.Files, packageFile{entry.name, entry.mode, fmt.Sprintf("%x", sha256.Sum256([]byte(entry.data)))})
	}
	return m
}

func makeArchive(t *testing.T, m packageManifest, entries []archiveEntry) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	zip := gzip.NewWriter(&buffer)
	tarball := tar.NewWriter(zip)
	for _, entry := range append([]archiveEntry{{manifestName, string(data), tar.TypeReg, 0644}}, entries...) {
		h := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: entry.kind}
		if entry.kind == tar.TypeSymlink || entry.kind == tar.TypeLink {
			h.Linkname = "bin/lumiqot"
		}
		if err := tarball.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tarball, entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestPackageValidation(t *testing.T) {
	for name, mutate := range map[string]func(*packageManifest, []archiveEntry) []archiveEntry{
		"valid":         func(m *packageManifest, e []archiveEntry) []archiveEntry { return e },
		"protocol":      func(m *packageManifest, e []archiveEntry) []archiveEntry { m.Lifecycle++; return e },
		"platform":      func(m *packageManifest, e []archiveEntry) []archiveEntry { m.Arch = "arm64"; return e },
		"wrong version": func(m *packageManifest, e []archiveEntry) []archiveEntry { m.Version = "v1.2.1"; return e },
		"no CLI":        func(m *packageManifest, e []archiveEntry) []archiveEntry { m.Files = m.Files[1:]; return e[1:] },
		"symlink": func(m *packageManifest, e []archiveEntry) []archiveEntry {
			e[0].kind, e[0].data = tar.TypeSymlink, ""
			return e
		},
		"hardlink": func(m *packageManifest, e []archiveEntry) []archiveEntry {
			e[0].kind, e[0].data = tar.TypeLink, ""
			return e
		},
		"traversal": func(m *packageManifest, e []archiveEntry) []archiveEntry { e[0].name = "../escaped"; return e },
		"absolute":  func(m *packageManifest, e []archiveEntry) []archiveEntry { e[0].name = "/escaped"; return e },
		"manifest traversal": func(m *packageManifest, e []archiveEntry) []archiveEntry {
			m.Files[1].Path = "resources/../escape"
			return e
		},
		"duplicate": func(m *packageManifest, e []archiveEntry) []archiveEntry { return append(e, e[0]) },
		"duplicate manifest path": func(m *packageManifest, e []archiveEntry) []archiveEntry {
			m.Files = append(m.Files, m.Files[0])
			return e
		},
		"missing":  func(m *packageManifest, e []archiveEntry) []archiveEntry { return e[:1] },
		"checksum": func(m *packageManifest, e []archiveEntry) []archiveEntry { e[0].data = "tampered"; return e },
		"setuid":   func(m *packageManifest, e []archiveEntry) []archiveEntry { e[0].mode = 04755; return e },
	} {
		t.Run(name, func(t *testing.T) {
			m := testManifest("v1.2.0")
			root := t.TempDir()
			if err := os.Chmod(root, 0755); err != nil {
				t.Fatal(err)
			}
			entries := mutate(&m, append([]archiveEntry(nil), validEntries...))
			data := makeArchive(t, m, entries)
			err := extractDistribution(bytes.NewReader(data), root, "v1.2.0", "amd64")
			if (err == nil) != (name == "valid") {
				t.Fatalf("extract error = %v", err)
			}
			if err == nil {
				if err := verifyDistribution(root, "v1.2.0", "amd64", false); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func releaseFixture(t *testing.T, tag string, badDigest bool) distributionRelease {
	t.Helper()
	archive := makeArchive(t, testManifest(tag), validEntries)
	name := "lumiqot-linux-" + tag + "-linux-amd64.tar.gz"
	digest := sha256.Sum256(archive)
	if badDigest {
		digest[0]++
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
		case "/" + tag + "/" + name:
			_, _ = w.Write(archive)
		case "/" + tag + "/lumiqot-linux-" + tag + "-SHA256SUMS":
			fmt.Fprintf(w, "%x  %s\n", digest, name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	release, err := latestDistributionRelease(server.Client(), server.URL+"/releases/latest", server.URL, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	return release
}

type fakeLifecycle struct {
	disabled bool
	store    distributionStore
	calls    []string
	running  string
	drained  bool
	failures map[string]error
}

func newFakeLifecycle(t *testing.T) *fakeLifecycle {
	t.Helper()
	root := t.TempDir()
	f := &fakeLifecycle{failures: map[string]error{}}
	f.store = distributionStore{root: root, link: filepath.Join(root, "lumiqot"), arch: "amd64", retain: 2, call: f.call}
	return f
}

func (f *fakeLifecycle) call(id, action string, timeout int) ([]byte, error) {
	if action == "version" {
		return []byte(packageVersion(id) + "\n"), f.failures[packageVersion(id)+" version"]
	}
	f.calls = append(f.calls, packageVersion(id)+" "+action)
	current, err := f.store.current()
	if err != nil {
		return nil, err
	}
	if id != current {
		return nil, fmt.Errorf("%s used CLI %s while %s was current", action, id, current)
	}
	key := packageVersion(id) + " " + action
	if action == "drain" && f.running == id {
		f.drained = true
	}
	if err := f.failures[key]; err != nil {
		return nil, err
	}
	switch action {
	case "start":
		f.drained = false
		if !f.disabled {
			f.running = id
		}
	case "stop":
		if f.running == id && !f.drained {
			return nil, errors.New("stopping active work")
		}
		f.running = ""
	case "status":
		state := "stopped"
		if f.disabled && f.running == "" {
			state = "disabled"
		}
		if f.running == id {
			state = "healthy"
			if f.drained {
				state = "draining"
			}
		}
		return []byte(fmt.Sprintf(`{"state":%q}`, state)), nil
	}
	return nil, nil
}

func (f *fakeLifecycle) apply(release distributionRelease) error {
	return f.store.locked(func(s distributionStore) error { return s.apply(release, nil, false) })
}

func (f *fakeLifecycle) mustApply(t *testing.T, tag string) string {
	t.Helper()
	if err := f.apply(releaseFixture(t, tag, false)); err != nil {
		t.Fatal(err)
	}
	id, err := f.store.current()
	if err != nil || packageVersion(id) != tag {
		t.Fatalf("current=%q err=%v", id, err)
	}
	return id
}

func (f *fakeLifecycle) expect(t *testing.T, version string, calls ...string) {
	t.Helper()
	if !reflect.DeepEqual(f.calls, calls) {
		t.Fatalf("called %v, want %v", f.calls, calls)
	}
	id, err := f.store.current()
	wantRunning := id
	if f.disabled {
		wantRunning = ""
	}
	if err != nil || packageVersion(id) != version || f.running != wantRunning || f.drained {
		t.Fatalf("active=%q running=%q draining=%v err=%v", id, f.running, f.drained, err)
	}
	state, err := f.store.state()
	if err != nil || state.Pending != nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	f.calls = nil
}

func TestDistributionInstallUpdateRetentionAndRollback(t *testing.T) {
	f := newFakeLifecycle(t)
	first := f.mustApply(t, "v1.0.0")
	f.expect(t, "v1.0.0", "v1.0.0 start", "v1.0.0 status")
	if !f.store.linkReady() {
		t.Fatal("public CLI link is missing")
	}
	second := f.mustApply(t, "v1.1.0")
	f.expect(t, "v1.1.0", "v1.0.0 drain", "v1.0.0 stop", "v1.1.0 start", "v1.1.0 status")
	third := f.mustApply(t, "v1.2.0")
	f.expect(t, "v1.2.0", "v1.1.0 drain", "v1.1.0 stop", "v1.2.0 start", "v1.2.0 status")
	if _, err := os.Stat(f.store.versionDir(first)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, kept := range []string{second, third} {
		if _, err := os.Stat(f.store.versionDir(kept)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.locked(func(s distributionStore) error { return s.rollback() }); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.1.0", "v1.2.0 drain", "v1.2.0 stop", "v1.1.0 start", "v1.1.0 status")
	if err := f.apply(releaseFixture(t, "v1.2.0", false)); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.1.0", "v1.1.0 status")
}

func TestDistributionApplyIdempotentAndRepairsInstall(t *testing.T) {
	f := newFakeLifecycle(t)
	id := f.mustApply(t, "v1.0.0")
	f.calls = nil
	if err := f.apply(releaseFixture(t, "v1.0.0", true)); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.0.0", "v1.0.0 status")
	if err := os.Remove(filepath.Join(f.store.versionDir(id), validEntries[1].name)); err != nil {
		t.Fatal(err)
	}
	next := f.mustApply(t, "v1.0.0")
	if next == id {
		t.Fatal("repair rewrote the active directory")
	}
	f.expect(t, "v1.0.0", "v1.0.0 drain", "v1.0.0 stop", "v1.0.0 start", "v1.0.0 status")
}

func TestDistributionFailures(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		badDigest     bool
		calls         []string
	}{
		{"bad checksum", "", true, nil},
		{"invalid CLI", "v1.1.0 version", false, nil},
		{"drain timeout", "v1.0.0 drain", false, []string{"v1.0.0 drain", "v1.0.0 start", "v1.0.0 status"}},
		{"stop failure", "v1.0.0 stop", false, []string{"v1.0.0 drain", "v1.0.0 stop", "v1.0.0 start", "v1.0.0 status"}},
		{"failed startup", "v1.1.0 start", false, []string{"v1.0.0 drain", "v1.0.0 stop", "v1.1.0 start", "v1.1.0 drain", "v1.1.0 stop", "v1.0.0 start", "v1.0.0 status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLifecycle(t)
			f.mustApply(t, "v1.0.0")
			f.calls = nil
			f.failures[tc.failure] = errors.New(tc.name)
			err := f.apply(releaseFixture(t, "v1.1.0", tc.badDigest))
			if err == nil {
				t.Fatal("expected failure")
			}
			f.expect(t, "v1.0.0", tc.calls...)
			state, err := f.store.state()
			if err != nil || (len(tc.calls) > 0 && !strings.Contains(state.LastError, tc.name)) {
				t.Fatalf("%+v %v", state, err)
			}
		})
	}
}

func TestDistributionInterruptedRecoveryAndLock(t *testing.T) {
	for _, switched := range []bool{false, true} {
		t.Run(fmt.Sprint(switched), func(t *testing.T) {
			f := newFakeLifecycle(t)
			old := f.mustApply(t, "v1.0.0")
			next := releaseFixture(t, "v1.1.0", false)
			f.calls = nil
			err := f.store.locked(func(s distributionStore) error {
				if err := f.store.locked(func(distributionStore) error { t.Fatal("second transaction acquired lock"); return nil }); err == nil {
					t.Fatal("concurrent update was accepted")
				}
				target, err := s.stage(next)
				if err != nil {
					return err
				}
				if err := s.save(installation{Pending: &packageTransaction{old, target, "activate"}}); err != nil {
					return err
				}
				if switched {
					if err := s.switchTo(target); err != nil {
						return err
					}
					f.running = target
				} else {
					f.running = ""
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.apply(next); err != nil {
				t.Fatal(err)
			}
			if switched {
				f.expect(t, "v1.0.0", "v1.1.0 drain", "v1.1.0 stop", "v1.0.0 start", "v1.0.0 status")
			} else {
				f.expect(t, "v1.0.0", "v1.0.0 start", "v1.0.0 status")
			}
		})
	}
}

func TestRecoveryFailureKeepsJournal(t *testing.T) {
	f := newFakeLifecycle(t)
	old := f.mustApply(t, "v1.0.0")
	f.failures["v1.1.0 start"] = errors.New("candidate failed")
	f.failures["v1.0.0 start"] = errors.New("rollback failed")
	err := f.apply(releaseFixture(t, "v1.1.0", false))
	if err == nil || !strings.Contains(err.Error(), "recovery failed") {
		t.Fatal(err)
	}
	state, err := f.store.state()
	if err != nil || state.Pending == nil || state.Pending.Old != old || !strings.Contains(state.RecoveryError, "rollback failed") || !strings.Contains(state.LastError, "candidate failed") {
		t.Fatalf("%+v %v", state, err)
	}
	if _, err := os.Stat(f.store.versionDir(state.Pending.Target)); err != nil {
		t.Fatal(err)
	}
	delete(f.failures, "v1.0.0 start")
	if err := f.apply(releaseFixture(t, "v1.1.0", false)); err != nil {
		t.Fatal(err)
	}
}

func TestFreshStartupFailureStopsCandidate(t *testing.T) {
	f := newFakeLifecycle(t)
	f.failures["v1.0.0 start"] = errors.New("first startup failed")
	if err := f.apply(releaseFixture(t, "v1.0.0", false)); err == nil {
		t.Fatal("expected startup failure")
	}
	f.expect(t, "", "v1.0.0 start", "v1.0.0 drain", "v1.0.0 stop")
	if f.store.linkReady() {
		t.Fatal("published CLI link after an unsuccessful first installation")
	}
}

func TestNoUpdateConvergesAfterManualDrain(t *testing.T) {
	f := newFakeLifecycle(t)
	f.mustApply(t, "v1.0.0")
	f.calls = nil
	f.drained = true
	if err := f.apply(releaseFixture(t, "v1.0.0", true)); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.0.0", "v1.0.0 status", "v1.0.0 start", "v1.0.0 status")
}

func TestDisabledDistributionInstallUpdateRollbackAndRecovery(t *testing.T) {
	f := newFakeLifecycle(t)
	f.disabled = true
	f.mustApply(t, "v1.0.0")
	f.expect(t, "v1.0.0", "v1.0.0 start", "v1.0.0 status")
	second := f.mustApply(t, "v1.1.0")
	f.expect(t, "v1.1.0", "v1.0.0 drain", "v1.0.0 stop", "v1.1.0 start", "v1.1.0 status")
	if err := f.store.locked(func(s distributionStore) error { return s.rollback() }); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.0.0", "v1.1.0 drain", "v1.1.0 stop", "v1.0.0 start", "v1.0.0 status")

	// Failed explicit activation must converge the original version.
	f.failures["v1.2.0 status"] = errors.New("candidate status unavailable")
	if err := f.apply(releaseFixture(t, "v1.2.0", false)); err == nil {
		t.Fatal("expected a candidate verification failure")
	}
	f.expect(t, "v1.0.0", "v1.0.0 drain", "v1.0.0 stop", "v1.2.0 start", "v1.2.0 status",
		"v1.2.0 drain", "v1.2.0 stop", "v1.0.0 start", "v1.0.0 status")

	// Simulate an interruption after switching current but before commit.
	err := f.store.locked(func(s distributionStore) error {
		old, err := s.current()
		if err != nil {
			return err
		}
		state, err := s.state()
		if err != nil {
			return err
		}
		state.Pending = &packageTransaction{Old: old, Target: second, Phase: "activate"}
		if err := s.save(state); err != nil {
			return err
		}
		return s.switchTo(second)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.apply(releaseFixture(t, "v1.2.0", false)); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "v1.0.0", "v1.1.0 drain", "v1.1.0 stop", "v1.0.0 start", "v1.0.0 status")
}

func TestStageLocalDistributionWithInstalledVersion(t *testing.T) {
	const tag = "v1.0.0"
	f := newFakeLifecycle(t)
	original := f.mustApply(t, tag)
	data := makeArchive(t, testManifest(tag), validEntries)
	err := f.store.locked(func(s distributionStore) error {
		if _, err := s.stageLocal(tag, bytes.NewReader(data), strings.Repeat("0", 64)); err == nil {
			return errors.New("local archive with an incorrect checksum was accepted")
		}
		target, err := s.stageLocal(tag, bytes.NewReader(data), fmt.Sprintf("%x", sha256.Sum256(data)))
		if err != nil {
			return err
		}
		if target == original {
			return errors.New("staging reused the active installation directory")
		}
		state, err := s.state()
		if err != nil {
			return err
		}
		return s.activate(state, original, target, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.store.state()
	if err != nil || state.Previous != original || f.running == original {
		t.Fatalf("state=%+v running=%q err=%v", state, f.running, err)
	}
	if err := f.store.locked(func(s distributionStore) error { return s.rollback() }); err != nil {
		t.Fatal(err)
	}
	if f.running != original {
		t.Fatalf("rollback started %q, want %q", f.running, original)
	}
}
