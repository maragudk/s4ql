// Command update vendors the official SQLite amalgamation into internal/sqlite/c, and verifies it.
//
// Usage, from the repository root:
//
//	go run ./internal/sqlite/update -version 3.54.1
//	go run ./internal/sqlite/update -verify
//
// With -version, the download URL and SHA3-256 checksum are read from the PRODUCT lines of
// https://sqlite.org/download.html, unless -url and -sha3 are given (needed for versions that are
// no longer on that page). The archive is downloaded, its size and SHA3-256 checksum are checked,
// the four files of the amalgamation are written to internal/sqlite/c, and internal/sqlite/c/source.json
// records the URL, size, and checksums.
//
// With -verify, the archive at the recorded URL is downloaded again, checked against the recorded
// size and checksums, and every vendored file is compared byte for byte with the archive.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	dir          = "internal/sqlite/c"
	manifestName = "source.json"
	downloadPage = "https://sqlite.org/download.html"
)

// files to vendor from the amalgamation archive.
var files = []string{"sqlite3.c", "sqlite3.h", "sqlite3ext.h", "shell.c"}

// manifest records where the vendored files came from.
type manifest struct {
	Version string            `json:"version"`
	URL     string            `json:"url"`
	Size    int               `json:"size"`
	SHA3256 string            `json:"sha3_256"`
	SHA256  string            `json:"sha256"`
	Files   map[string]string `json:"files_sha256"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	version := flag.String("version", "", "SQLite version to vendor, such as 3.54.1")
	url := flag.String("url", "", "download URL of the amalgamation zip, if not on the download page")
	sum := flag.String("sha3", "", "SHA3-256 checksum of the zip, required with -url")
	verify := flag.Bool("verify", false, "verify the vendored files against the recorded source")
	flag.Parse()

	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}

	if *verify {
		return verifyVendored()
	}

	if *version == "" {
		return errors.New("give -version or -verify")
	}

	if (*url == "") != (*sum == "") {
		return errors.New("give both -url and -sha3, or neither")
	}

	size := -1
	if *url == "" {
		page, err := get(downloadPage)
		if err != nil {
			return err
		}
		p, err := findProduct(page, *version)
		if err != nil {
			return err
		}
		*url, size, *sum = "https://sqlite.org/"+p.path, p.size, p.sha3
	}

	archive, err := get(*url)
	if err != nil {
		return err
	}
	if size >= 0 && len(archive) != size {
		return fmt.Errorf("size of %v is %v, want %v", *url, len(archive), size)
	}
	if got := sha3Hex(archive); got != strings.ToLower(*sum) {
		return fmt.Errorf("SHA3-256 of %v is %v, want %v", *url, got, *sum)
	}

	contents, err := extract(archive, *version)
	if err != nil {
		return err
	}

	m := manifest{
		Version: *version,
		URL:     *url,
		Size:    len(archive),
		SHA3256: sha3Hex(archive),
		SHA256:  sha256Hex(archive),
		Files:   map[string]string{},
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), contents[name], 0o644); err != nil {
			return err
		}
		m.Files[name] = sha256Hex(contents[name])
	}

	b, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), append(b, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("Vendored SQLite %v from %v\n", *version, *url)
	return nil
}

func verifyVendored() error {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}

	archive, err := get(m.URL)
	if err != nil {
		return err
	}
	if len(archive) != m.Size {
		return fmt.Errorf("size of %v is %v, recorded %v", m.URL, len(archive), m.Size)
	}
	if got := sha3Hex(archive); got != m.SHA3256 {
		return fmt.Errorf("SHA3-256 of %v is %v, recorded %v", m.URL, got, m.SHA3256)
	}
	if got := sha256Hex(archive); got != m.SHA256 {
		return fmt.Errorf("SHA-256 of %v is %v, recorded %v", m.URL, got, m.SHA256)
	}

	contents, err := extract(archive, m.Version)
	if err != nil {
		return err
	}
	for _, name := range files {
		local, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(local, contents[name]) {
			return fmt.Errorf("%v differs from the archive", name)
		}
		if got := sha256Hex(local); got != m.Files[name] {
			return fmt.Errorf("SHA-256 of %v is %v, recorded %v", name, got, m.Files[name])
		}
	}

	fmt.Printf("Verified SQLite %v against %v\n", m.Version, m.URL)
	return nil
}

type product struct {
	path string
	size int
	sha3 string
}

var productLine = regexp.MustCompile(`(?m)^PRODUCT,([^,]+),([^,]+),(\d+),([0-9a-f]{64})$`)

// findProduct in the PRODUCT lines of the SQLite download page, for the amalgamation zip of the given version.
func findProduct(page []byte, version string) (product, error) {
	number, err := versionNumber(version)
	if err != nil {
		return product{}, err
	}
	name := fmt.Sprintf("sqlite-amalgamation-%v.zip", number)

	for _, match := range productLine.FindAllSubmatch(page, -1) {
		if string(match[1]) != version || filepath.Base(string(match[2])) != name {
			continue
		}
		size, err := strconv.Atoi(string(match[3]))
		if err != nil {
			return product{}, err
		}
		return product{path: string(match[2]), size: size, sha3: string(match[4])}, nil
	}
	return product{}, fmt.Errorf("%v not on %v; give -url and -sha3", name, downloadPage)
}

// versionNumber in the form SQLite uses in file names, such as 3540100 for 3.54.1.
func versionNumber(version string) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("version %q is not of the form X.Y.Z", version)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 99 {
			return 0, fmt.Errorf("version %q is not of the form X.Y.Z", version)
		}
		n[i] = v
	}
	return n[0]*1000000 + n[1]*10000 + n[2]*100, nil
}

// extract the vendored files from the amalgamation archive.
func extract(archive []byte, version string) (map[string][]byte, error) {
	number, err := versionNumber(version)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("sqlite-amalgamation-%v/", number)

	r, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}

	contents := map[string][]byte{}
	for _, f := range r.File {
		name, ok := strings.CutPrefix(f.Name, prefix)
		if !ok {
			continue
		}
		for _, want := range files {
			if name != want {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return nil, err
			}
			contents[name] = b
		}
	}

	for _, name := range files {
		if _, ok := contents[name]; !ok {
			return nil, fmt.Errorf("%v%v not in archive", prefix, name)
		}
	}
	return contents, nil
}

func get(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %v: %v", url, res.Status)
	}
	return io.ReadAll(res.Body)
}

func sha3Hex(b []byte) string {
	sum := sha3.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
