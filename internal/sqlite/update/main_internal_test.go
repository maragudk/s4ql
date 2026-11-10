package main

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"maragu.dev/is"
)

func TestVersionNumber(t *testing.T) {
	tests := []struct {
		version  string
		expected int
	}{
		{version: "3.54.1", expected: 3540100},
		{version: "3.8.11", expected: 3081100},
		{version: "3.100.0", expected: 0},
		{version: "3.54", expected: 0},
		{version: "3.54.x", expected: 0},
	}

	for _, test := range tests {
		t.Run("should encode "+test.version+" as in SQLite file names, or fail", func(t *testing.T) {
			n, err := versionNumber(test.version)
			if test.expected == 0 {
				is.True(t, err != nil)
				return
			}
			is.NotError(t, err)
			is.Equal(t, test.expected, n)
		})
	}
}

func TestFindProduct(t *testing.T) {
	page := []byte(`<!--
Download product data for scripts to read
PRODUCT,VERSION,RELATIVE-URL,SIZE-IN-BYTES,SHA3-HASH
PRODUCT,3.54.1,2026/sqlite-src-3540100.zip,14899528,877c6f4996ce7adb368686bce21638de9e3d91534832ad3d9d119037d1bc22ad
PRODUCT,3.54.1,2026/sqlite-amalgamation-3540100.zip,2990966,92281c86e2957f9b9305890f9ac04204caa7b677cc868dc326d942689df28e30
-->`)

	t.Run("should find the amalgamation zip of the given version", func(t *testing.T) {
		p, err := findProduct(page, "3.54.1")
		is.NotError(t, err)
		is.Equal(t, "2026/sqlite-amalgamation-3540100.zip", p.path)
		is.Equal(t, 2990966, p.size)
		is.Equal(t, "92281c86e2957f9b9305890f9ac04204caa7b677cc868dc326d942689df28e30", p.sha3)
	})

	t.Run("should fail for a version not on the page", func(t *testing.T) {
		_, err := findProduct(page, "3.54.0")
		is.True(t, err != nil)
	})
}

func TestCheckURL(t *testing.T) {
	t.Run("should accept the amalgamation of the version on sqlite.org", func(t *testing.T) {
		is.NotError(t, checkURL("https://sqlite.org/2026/sqlite-amalgamation-3540100.zip", "3.54.1"))
	})

	t.Run("should refuse another host, another version, or another file", func(t *testing.T) {
		is.True(t, checkURL("https://example.com/2026/sqlite-amalgamation-3540100.zip", "3.54.1") != nil)
		is.True(t, checkURL("http://sqlite.org/2026/sqlite-amalgamation-3540100.zip", "3.54.1") != nil)
		is.True(t, checkURL("https://sqlite.org/2026/sqlite-amalgamation-3540000.zip", "3.54.1") != nil)
		is.True(t, checkURL("https://sqlite.org/2026/sqlite-src-3540100.zip", "3.54.1") != nil)
	})
}

func TestCheckArchive(t *testing.T) {
	archive := []byte("archive")
	sha3Sum, sha256Sum := sha3Hex(archive), sha256Hex(archive)

	t.Run("should accept the right size and checksums", func(t *testing.T) {
		is.NotError(t, checkArchive(archive, len(archive), sha3Sum, sha256Sum))
		is.NotError(t, checkArchive(archive, -1, sha3Sum, ""))
	})

	t.Run("should refuse a wrong size or checksum", func(t *testing.T) {
		is.True(t, checkArchive(archive, len(archive)+1, sha3Sum, sha256Sum) != nil)
		is.True(t, checkArchive(archive, len(archive), sha256Sum, sha256Sum) != nil)
		is.True(t, checkArchive(archive, len(archive), sha3Sum, sha3Sum) != nil)
		is.True(t, checkArchive(archive, len(archive), "", "") != nil)
	})
}

func TestCheckVendored(t *testing.T) {
	contents := map[string][]byte{
		"sqlite3.c":    []byte("c"),
		"sqlite3.h":    []byte("h"),
		"sqlite3ext.h": []byte("ext"),
		"shell.c":      []byte("shell"),
	}
	archive := zipOf(t, "sqlite-amalgamation-3540100/", contents)
	m := manifest{Version: "3.54.1", Files: map[string]string{}}
	for name, b := range contents {
		m.Files[name] = sha256Hex(b)
	}

	t.Run("should accept files and a header that match the archive and the manifest", func(t *testing.T) {
		is.NotError(t, checkVendored(m, archive, contents, header(m)))
	})

	t.Run("should refuse a file that differs from the archive", func(t *testing.T) {
		local := map[string][]byte{}
		for name, b := range contents {
			local[name] = b
		}
		local["shell.c"] = []byte("changed")
		err := checkVendored(m, archive, local, header(m))
		is.Equal(t, "shell.c differs from the archive", err.Error())
	})

	t.Run("should refuse a manifest whose checksums do not match", func(t *testing.T) {
		wrong := manifest{Version: m.Version, Files: map[string]string{}}
		for name, sum := range m.Files {
			wrong.Files[name] = sum
		}
		wrong.Files["sqlite3.c"] = sha256Hex([]byte("other"))
		err := checkVendored(wrong, archive, contents, header(wrong))
		is.True(t, strings.HasPrefix(err.Error(), "SHA-256 of sqlite3.c is"))
	})

	t.Run("should refuse a header that does not match the manifest", func(t *testing.T) {
		err := checkVendored(m, archive, contents, []byte("#define S4QL_SQLITE_VERSION \"3.54.1\"\n"))
		is.Equal(t, "source.h does not match the manifest; run the update again", err.Error())
	})

	t.Run("should refuse an archive without the vendored files", func(t *testing.T) {
		err := checkVendored(m, zipOf(t, "other/", contents), contents, header(m))
		is.True(t, err != nil)
	})
}

func TestHeader(t *testing.T) {
	t.Run("should define the version and the SHA-256 of each vendored file", func(t *testing.T) {
		h := string(header(manifest{Version: "3.54.1", Files: map[string]string{
			"sqlite3.c": "a", "sqlite3.h": "b", "sqlite3ext.h": "c", "shell.c": "d",
		}}))
		is.True(t, strings.Contains(h, "#define S4QL_SQLITE_VERSION \"3.54.1\"\n"))
		is.True(t, strings.Contains(h, "#define S4QL_SHA256_SQLITE3_C \"a\"\n"))
		is.True(t, strings.Contains(h, "#define S4QL_SHA256_SQLITE3_H \"b\"\n"))
		is.True(t, strings.Contains(h, "#define S4QL_SHA256_SQLITE3EXT_H \"c\"\n"))
		is.True(t, strings.Contains(h, "#define S4QL_SHA256_SHELL_C \"d\"\n"))
	})
}

func zipOf(t *testing.T, prefix string, contents map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, content := range contents {
		f, err := w.Create(prefix + name)
		is.NotError(t, err)
		_, err = f.Write(content)
		is.NotError(t, err)
	}
	is.NotError(t, w.Close())
	return b.Bytes()
}
