package format_test

import (
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
)

func TestName_String(t *testing.T) {
	t.Run("should be 64 lowercase hexadecimal digits", func(t *testing.T) {
		n := format.Name(sha256.Sum256([]byte("abc")))
		is.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", n.String())
	})

	t.Run("should be 64 zeros for the empty log", func(t *testing.T) {
		is.Equal(t, strings.Repeat("0", 64), format.EmptyLog.String())
	})
}

func TestParseName(t *testing.T) {
	t.Run("should parse what String writes", func(t *testing.T) {
		n := format.Name(sha256.Sum256([]byte("abc")))
		parsed, err := format.ParseName(n.String())
		is.NotError(t, err)
		is.Equal(t, n, parsed)
	})

	tests := []struct {
		name  string
		input string
	}{
		{name: "should refuse uppercase digits", input: "BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD"},
		{name: "should refuse 63 digits", input: strings.Repeat("0", 63)},
		{name: "should refuse 65 digits", input: strings.Repeat("0", 65)},
		{name: "should refuse an empty string", input: ""},
		{name: "should refuse a character that is not a digit", input: strings.Repeat("0", 63) + "g"},
		{name: "should refuse a sign", input: "+" + strings.Repeat("0", 63)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := format.ParseName(test.input)
			is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed")
		})
	}
}

func TestNameFromBytes(t *testing.T) {
	t.Run("should take 32 bytes", func(t *testing.T) {
		b := sha256.Sum256(nil)
		n, err := format.NameFromBytes(b[:])
		is.NotError(t, err)
		is.Equal(t, format.Name(b), n)
	})

	t.Run("should refuse 31 bytes", func(t *testing.T) {
		_, err := format.NameFromBytes(make([]byte, 31))
		is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed")
	})

	t.Run("should refuse no bytes", func(t *testing.T) {
		_, err := format.NameFromBytes(nil)
		is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed")
	})
}

func TestName_MarshalText(t *testing.T) {
	t.Run("should be a JSON string of the hexadecimal digits, and read back", func(t *testing.T) {
		n := format.Name(sha256.Sum256([]byte("abc")))
		b, err := json.Marshal(n)
		is.NotError(t, err)
		is.Equal(t, `"`+n.String()+`"`, string(b))

		var parsed format.Name
		err = json.Unmarshal(b, &parsed)
		is.NotError(t, err)
		is.Equal(t, n, parsed)
	})
}
