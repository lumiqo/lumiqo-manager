package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	manifestName = "manifest.json"
	packageCLI   = "bin/lumiqot"
	maxArchive   = 128 << 20
	maxPayload   = 512 << 20
	maxManifest  = 1 << 20
	maxEntries   = 4096
)

const lifecycleProtocol = 2

type packageManifest struct {
	Format    int           `json:"format"`
	Package   string        `json:"package"`
	Version   string        `json:"version"`
	OS        string        `json:"os"`
	Arch      string        `json:"arch"`
	CLI       string        `json:"cli"`
	Lifecycle int           `json:"lifecycle"`
	Files     []packageFile `json:"files"`
}

type packageFile struct {
	Path   string `json:"path"`
	Mode   int64  `json:"mode"`
	SHA256 string `json:"sha256"`
}

var packagePath = regexp.MustCompile(`^[a-zA-Z0-9._+@/-]+$`)

func validPackagePath(name string) bool {
	return len(name) <= 512 && packagePath.MatchString(name) && fs.ValidPath(name) && name != "."
}

func readManifest(r io.Reader, version, arch string) (map[string]packageFile, error) {
	limited := &io.LimitedReader{R: r, N: maxManifest + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var manifest packageManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode package manifest: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || limited.N == 0 {
		return nil, errors.New("invalid data after package manifest")
	}
	if manifest.Format != 1 || manifest.Lifecycle != lifecycleProtocol || manifest.CLI != packageCLI ||
		manifest.Package != "lumiqot-linux" || manifest.OS != "linux" ||
		manifest.Version != version || !stableDistributionTag.MatchString(version) ||
		manifest.Arch != arch || (arch != "amd64" && arch != "arm64") ||
		len(manifest.Files) == 0 || len(manifest.Files) >= maxEntries {
		return nil, errors.New("unsupported package manifest identity, platform, layout, or protocol")
	}
	files := make(map[string]packageFile, len(manifest.Files))
	for _, file := range manifest.Files {
		digest, err := hex.DecodeString(file.SHA256)
		if !validPackagePath(file.Path) || file.Path == manifestName ||
			(file.Mode != 0644 && file.Mode != 0755) || err != nil || len(digest) != sha256.Size ||
			file.SHA256 != strings.ToLower(file.SHA256) {
			return nil, fmt.Errorf("invalid manifest entry %q", file.Path)
		}
		if _, exists := files[file.Path]; exists {
			return nil, fmt.Errorf("duplicate manifest path %q", file.Path)
		}
		files[file.Path] = file
	}
	if files[packageCLI].Mode != 0755 {
		return nil, errors.New("manifest requires executable bin/lumiqot")
	}
	for name := range files {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists || parent == manifestName {
				return nil, fmt.Errorf("manifest path conflicts with directory %q", parent)
			}
		}
	}
	return files, nil
}

func extractDistribution(r io.Reader, root, version, arch string) error {
	source := bufio.NewReader(r)
	compressed, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer compressed.Close()
	compressed.Multistream(false)
	limit := &io.LimitedReader{R: compressed, N: maxPayload + 1}
	archive := tar.NewReader(limit)
	header, err := archive.Next()
	if err != nil {
		return err
	}
	if header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Mode != 0644 || header.Linkname != "" || header.Size > maxManifest {
		return errors.New("expected regular manifest.json as first archive entry")
	}
	data, err := io.ReadAll(archive)
	if err != nil {
		return err
	}
	files, err := readManifest(strings.NewReader(string(data)), version, arch)
	if err != nil {
		return err
	}
	if err := writePayload(root, manifestName, 0644, strings.NewReader(string(data)), ""); err != nil {
		return err
	}
	var payloadSize int64
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Size < 0 || header.Size > maxPayload-payloadSize {
			return errors.New("unpacked archive exceeds size limit")
		}
		payloadSize += header.Size
		entry, declared := files[header.Name]
		if !declared || header.Typeflag != tar.TypeReg || header.Mode != entry.Mode || header.Linkname != "" {
			return fmt.Errorf("unexpected archive entry %q", header.Name)
		}
		if err := writePayload(root, entry.Path, fs.FileMode(entry.Mode), archive, entry.SHA256); err != nil {
			return err
		}
		delete(files, header.Name)
	}
	if len(files) != 0 {
		return errors.New("archive is missing manifest entries")
	}
	// Read through the gzip checksum. Only tar zero padding may follow EOF.
	var padding [4096]byte
	for {
		n, err := limit.Read(padding[:])
		for _, b := range padding[:n] {
			if b != 0 {
				return errors.New("unexpected data after tar archive")
			}
		}
		if limit.N == 0 {
			return errors.New("unpacked archive exceeds size limit")
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if _, err := source.ReadByte(); err != io.EOF {
		return errors.New("unexpected data after gzip stream")
	}
	return filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			if name != root {
				err = os.Chmod(name, 0755)
			}
			return errors.Join(err, syncDirectory(name))
		}
		return err
	})
}

func writePayload(root, name string, mode fs.FileMode, data io.Reader, expected string) error {
	target := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(file, hash), data)
	if err := errors.Join(err, file.Chmod(mode), file.Sync(), file.Close()); err != nil {
		return err
	}
	if expected != "" && hex.EncodeToString(hash.Sum(nil)) != expected {
		return fmt.Errorf("payload SHA-256 mismatch for %s", name)
	}
	return nil
}

func verifyDistribution(root, version, arch string, cliOnly bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0755 {
		return errors.New("invalid version directory")
	}
	manifest := filepath.Join(root, manifestName)
	info, err = os.Lstat(manifest)
	if err != nil {
		return err
	}
	if info.Mode() != 0644 {
		return errors.New("invalid installed manifest mode")
	}
	file, err := os.Open(manifest)
	if err != nil {
		return err
	}
	defer file.Close()
	files, err := readManifest(file, version, arch)
	if err != nil {
		return err
	}
	verify := func(name string, entry packageFile) error {
		info, err := os.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode() != fs.FileMode(entry.Mode) {
			return fmt.Errorf("invalid mode for %s", entry.Path)
		}
		file, err := os.Open(name)
		if err != nil {
			return err
		}
		defer file.Close()
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(file, maxPayload+1))
		if err != nil {
			return err
		}
		if n > maxPayload || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("installed SHA-256 mismatch for %s", entry.Path)
		}
		return nil
	}
	if cliOnly {
		bin, err := os.Lstat(filepath.Join(root, "bin"))
		if err != nil {
			return err
		}
		if bin.Mode() != fs.ModeDir|0755 {
			return errors.New("invalid installed bin directory")
		}
		return verify(filepath.Join(root, packageCLI), files[packageCLI])
	}
	dirs := map[string]bool{".": true}
	for name := range files {
		for parent := path.Dir(name); !dirs[parent]; parent = path.Dir(parent) {
			dirs[parent] = true
		}
	}
	err = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !dirs[relative] || info.Mode() != fs.ModeDir|0755 {
				return fmt.Errorf("unexpected directory %q", relative)
			}
			return nil
		}
		if relative == manifestName {
			return nil
		}
		declared, ok := files[relative]
		if !ok {
			return fmt.Errorf("unlisted installed file %q", relative)
		}
		delete(files, relative)
		return verify(name, declared)
	})
	if err == nil && len(files) != 0 {
		return errors.New("installed package files are missing")
	}
	return err
}

func (r distributionRelease) stage(root string) error {
	digest, err := r.checksum()
	if err != nil {
		return err
	}
	response, err := r.openAsset(r.archiveName() + ".tar.gz")
	if err != nil {
		return err
	}
	defer response.Close()
	return r.stageArchive(root, response, digest)
}

func (r distributionRelease) stageArchive(root string, source io.Reader, digest string) error {
	archive, err := os.CreateTemp(filepath.Dir(root), ".download-")
	if err != nil {
		return err
	}
	defer archive.Close()
	// Keep the archive private and automatically reclaim it on interruption.
	if err := os.Remove(archive.Name()); err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(source, maxArchive+1))
	if err != nil {
		return err
	}
	if n > maxArchive || hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("distribution archive size or SHA-256 mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractDistribution(archive, root, r.tag, r.arch)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
