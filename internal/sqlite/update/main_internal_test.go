package main

import (
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
