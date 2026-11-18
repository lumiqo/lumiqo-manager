package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerPayloadPermissions(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"bin", "libexec/lumiqot", "licenses"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]os.FileMode{
		"bin/lumiqot":                   0755,
		"libexec/lumiqot/lumiqo-worker": 0755,
		"licenses/LICENSE":              0644,
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := makeWorkerReadable(root); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has permissions %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	info, err := os.Stat(filepath.Join(root, "libexec/lumiqot"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("directory has permissions %o", info.Mode().Perm())
	}
}
