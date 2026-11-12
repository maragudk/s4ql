package format_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
)

var update = flag.Bool("update", false, "write the sample files in testdata again")

var testHead = format.Name(sha256.Sum256([]byte("head"))).String()

func TestParseManifest(t *testing.T) {
	t.Run("should read a manifest with a lease", func(t *testing.T) {
		m, err := format.ParseManifest([]byte(`{
			"version": 0,
			"branches": {"main": {"head": "` + testHead + `"}},
			"lease": {"holder": "5b0e77c3", "expires": "2026-11-11T12:00:30.123Z"}
		}`))
		is.NotError(t, err)
		is.Equal(t, testHead, m.Head.String())
		is.NotNil(t, m.Lease)
		is.Equal(t, "5b0e77c3", m.Lease.Holder)
		is.True(t, time.Date(2026, 11, 11, 12, 0, 30, 123_000_000, time.UTC).Equal(m.Lease.Expires))
	})

	t.Run("should read a manifest without a lease", func(t *testing.T) {
		m, err := format.ParseManifest([]byte(`{"version":0,"branches":{"main":{"head":"` + strings.Repeat("0", 64) + `"}}}`))
		is.NotError(t, err)
		is.Equal(t, format.EmptyLog, m.Head)
		is.True(t, m.Lease == nil, "lease is not nil")
	})

	t.Run("should read any RFC 3339 time as the expiry", func(t *testing.T) {
		want := time.Date(2026, 11, 11, 12, 0, 30, 0, time.UTC)
		for _, expires := range []string{
			"2026-11-11T12:00:30Z",
			"2026-11-11T12:00:30.000000000Z",
			"2026-11-11T13:00:30+01:00",
			"2026-11-11T11:30:30-00:30",
			"2026-11-11t12:00:30z",
		} {
			m, err := format.ParseManifest(manifestWithLease(`{"holder":"a","expires":"` + expires + `"}`))
			is.NotError(t, err, expires)
			is.True(t, want.Equal(m.Lease.Expires), expires)
		}
	})

	t.Run("should read an expiry with more than nine digits of fraction to the nanosecond", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30.1234567899Z"}`))
		is.NotError(t, err)
		is.True(t, time.Date(2026, 11, 11, 12, 0, 30, 123_456_789, time.UTC).Equal(m.Lease.Expires))
	})

	t.Run("should keep the fields it does not know at every level", func(t *testing.T) {
		input := `{
			"version": 0,
			"zebra": [1, {"b": 2}],
			"apple": "x",
			"branches": {
				"main": {"head": "` + testHead + `", "since": 3},
				"other": {"head": "y"}
			},
			"lease": {"holder": "h", "expires": "2026-11-11T12:00:30.123Z", "epoch": 7}
		}`
		m, err := format.ParseManifest([]byte(input))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.Equal(t, `{
  "version": 0,
  "branches": {
    "main": {
      "head": "`+testHead+`",
      "since": 3
    },
    "other": {
      "head": "y"
    }
  },
  "lease": {
    "holder": "h",
    "expires": "2026-11-11T12:00:30.123Z",
    "epoch": 7
  },
  "apple": "x",
  "zebra": [
    1,
    {
      "b": 2
    }
  ]
}
`, string(b))
	})

	t.Run("should keep the strings and numbers of the fields it does not know as they are written", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30Z","x":["\u0041\/<&>",1.50,1E+2,-0]}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"\u0041\/<&>",
      1.50,
      1E+2,
      -0`)), string(b))
	})

	t.Run("should write the fields it does not know in the order of their keys at every level", func(t *testing.T) {
		unknown := `"é":0,"b":0,"_":0,"B":0,"a":0`
		m, err := format.ParseManifest([]byte(`{"version":0,` + unknown + `,` +
			`"branches":{"b":0,"main":{"head":"` + testHead + `",` + unknown + `},"é":0,"_":0,"B":0,"a":0},` +
			`"lease":{"holder":"h",` + unknown + `,"expires":"2026-11-11T12:00:30Z"}}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		v := jsontext.Value(b)
		is.NotError(t, v.Compact())
		sorted := `"B":0,"_":0,"a":0,"b":0,"é":0`
		is.Equal(t, `{"version":0,"branches":{"main":{"head":"`+testHead+`",`+sorted+`},`+sorted+`},`+
			`"lease":{"holder":"h","expires":"2026-11-11T12:00:30.000Z",`+sorted+`},`+sorted+`}`, string(v))
	})

	t.Run("should escape U+2028 and U+2029 in the fields it does not know, and keep the rest of their strings", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30Z","x":"a` + "\u2028" + `b` + "\u2029" + `c\/"}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"x": "a\u2028b\u2029c\/"`)), string(b))
	})

	t.Run("should not change when the bytes it was read from change", func(t *testing.T) {
		input := []byte(`{"version":0,"x":"abc","branches":{"main":{"head":"` + testHead + `","x":"abc"},"x":"abc"},` +
			`"lease":{"holder":"h","expires":"2026-11-11T12:00:30Z","x":"abc"}}`)
		m, err := format.ParseManifest(input)
		is.NotError(t, err)
		want, err := m.Marshal()
		is.NotError(t, err)

		for i := range input {
			input[i] = ' '
		}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.Equal(t, string(want), string(b))
	})

	t.Run("should keep the fields it does not know after the head and the lease are changed", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30Z","epoch":7}`))
		is.NotError(t, err)
		m.Head = format.Name(sha256.Sum256([]byte("next")))
		m.Lease.Expires = m.Lease.Expires.Add(time.Second)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"epoch": 7`)), "epoch is gone")
		is.True(t, bytes.Contains(b, []byte(`"expires": "2026-11-11T12:00:31.000Z"`)), "expiry is wrong")
		is.True(t, bytes.Contains(b, []byte(m.Head.String())), "head is wrong")
	})

	newer := []struct {
		name  string
		input string
	}{
		{name: "should refuse version 1 as newer", input: `{"version":1,"branches":{"main":{"head":"` + strings.Repeat("0", 64) + `"}}}`},
		{name: "should refuse version 1 as newer even when the rest is not understood", input: `{"version":1,"branches":[]}`},
		{name: "should refuse a large version as newer", input: `{"version":123456789012345678901234567890}`},
		{name: "should refuse version 1 as newer even when the lease is null", input: `{"version":1,"lease":null}`},
		{name: "should refuse version 1 as newer even when the lease is not an object", input: `{"version":1,"lease":5}`},
	}
	for _, test := range newer {
		t.Run(test.name, func(t *testing.T) {
			_, err := format.ParseManifest([]byte(test.input))
			is.True(t, errors.Is(err, format.ErrNewerVersion), "error is not newer version: "+errorString(err))
			is.True(t, !errors.Is(err, format.ErrMalformed), "error is malformed")
		})
	}

	zeros := strings.Repeat("0", 64)
	malformed := []struct {
		name  string
		input string
	}{
		{name: "should refuse an empty document", input: ``},
		{name: "should refuse a document that is not JSON", input: `{"version":0,`},
		{name: "should refuse a document that is not an object", input: `[]`},
		{name: "should refuse data after the object", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}}} {}`},
		{name: "should refuse a missing version", input: `{"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version written with another case", input: `{"Version":0,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version that is a string", input: `{"version":"0","branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version that is negative", input: `{"version":-1,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version with a fraction", input: `{"version":0.0,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version with an exponent", input: `{"version":0e0,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version that is null", input: `{"version":null,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a version given twice", input: `{"version":0,"version":1,"branches":{"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse missing branches", input: `{"version":0}`},
		{name: "should refuse branches that are not an object", input: `{"version":0,"branches":[]}`},
		{name: "should refuse a missing main branch", input: `{"version":0,"branches":{"other":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a main branch that is not an object", input: `{"version":0,"branches":{"main":"` + zeros + `"}}`},
		{name: "should refuse a main branch given twice", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"},"main":{"head":"` + zeros + `"}}}`},
		{name: "should refuse a missing head", input: `{"version":0,"branches":{"main":{}}}`},
		{name: "should refuse a head that is not a string", input: `{"version":0,"branches":{"main":{"head":0}}}`},
		{name: "should refuse a head with uppercase digits", input: `{"version":0,"branches":{"main":{"head":"` + strings.ToUpper(testHead) + `"}}}`},
		{name: "should refuse a head of 63 digits", input: `{"version":0,"branches":{"main":{"head":"` + zeros[1:] + `"}}}`},
		{name: "should refuse a head given twice", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `","head":"` + zeros + `"}}}`},
		{name: "should refuse a lease that is null", input: string(manifestWithLease(`null`))},
		{name: "should refuse a lease that is not an object", input: string(manifestWithLease(`"h"`))},
		{name: "should refuse a lease without a holder", input: string(manifestWithLease(`{"expires":"2026-11-11T12:00:30Z"}`))},
		{name: "should refuse a lease with an empty holder", input: string(manifestWithLease(`{"holder":"","expires":"2026-11-11T12:00:30Z"}`))},
		{name: "should refuse a lease whose holder is not a string", input: string(manifestWithLease(`{"holder":1,"expires":"2026-11-11T12:00:30Z"}`))},
		{name: "should refuse a lease without an expiry", input: string(manifestWithLease(`{"holder":"h"}`))},
		{name: "should refuse a lease whose expiry is not a string", input: string(manifestWithLease(`{"holder":"h","expires":1}`))},
		{name: "should refuse an expiry without a time zone", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30"}`))},
		{name: "should refuse an expiry that is only a date", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11"}`))},
		{name: "should refuse an expiry with a comma before the fraction", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30,123Z"}`))},
		{name: "should refuse an expiry with a dot and no fraction", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30.Z"}`))},
		{name: "should refuse an expiry with an offset of 24 hours", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30+24:00"}`))},
		{name: "should refuse an expiry with an offset of 60 minutes", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30+23:60"}`))},
		{name: "should refuse an expiry with an offset without a colon", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T12:00:30+0100"}`))},
		{name: "should refuse an expiry with a month of one digit", input: string(manifestWithLease(`{"holder":"h","expires":"2026-1-11T12:00:30Z"}`))},
		{name: "should refuse an expiry at hour 24", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11T24:00:00Z"}`))},
		{name: "should refuse an expiry with a leap second", input: string(manifestWithLease(`{"holder":"h","expires":"2026-12-31T23:59:60Z"}`))},
		{name: "should refuse an expiry with a space for the T", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-11 12:00:30Z"}`))},
		{name: "should refuse an expiry on a day that does not exist", input: string(manifestWithLease(`{"holder":"h","expires":"2026-02-30T12:00:30Z"}`))},
		{name: "should refuse an expiry before year 0 in UTC", input: string(manifestWithLease(`{"holder":"h","expires":"0000-01-01T00:00:00+01:00"}`))},
		{name: "should refuse an expiry after year 9999 in UTC", input: string(manifestWithLease(`{"holder":"h","expires":"9999-12-31T23:59:59-01:00"}`))},
		{name: "should refuse a lease given twice", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"lease":{"holder":"h","expires":"2026-11-11T12:00:30Z"},"lease":{"holder":"h","expires":"2026-11-11T12:00:30Z"}}`},
		{name: "should refuse an unknown key given twice", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":1,"x":2}`},
		{name: "should refuse a key given twice in an object it does not know", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":{"a":1,"a":2}}`},
		{name: "should refuse a key given twice in an object it does not know, also in a newer version", input: `{"version":1,"x":{"a":1,"a":2}}`},
		{name: "should refuse a key given twice in another spelling", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":1,"\u0078":2}`},
		{name: "should refuse bytes that are not valid UTF-8", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":"` + "\xff" + `"}`},
		{name: "should refuse an escaped lone surrogate", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":"\ud800"}`},
		{name: "should refuse an escaped lone low surrogate", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":"\udc00"}`},
		{name: "should refuse an escaped low surrogate first", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"x":"\udc00\ud800"}`},
	}
	for _, test := range malformed {
		t.Run(test.name, func(t *testing.T) {
			_, err := format.ParseManifest([]byte(test.input))
			is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed: "+errorString(err))
			is.True(t, !errors.Is(err, format.ErrNewerVersion), "error is newer version")
		})
	}

	t.Run("should read an escaped surrogate pair", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"\ud83d\ude00","expires":"2026-11-11T12:00:30Z"}`))
		is.NotError(t, err)
		is.Equal(t, "\U0001F600", m.Lease.Holder)
	})
}

func TestManifest_Marshal(t *testing.T) {
	t.Run("should write a new manifest with the empty log as head and no lease", func(t *testing.T) {
		b, err := format.Manifest{}.Marshal()
		is.NotError(t, err)
		is.Equal(t, `{
  "version": 0,
  "branches": {
    "main": {
      "head": "`+strings.Repeat("0", 64)+`"
    }
  }
}
`, string(b))
	})

	t.Run("should write the expiry in UTC with fractions of a second", func(t *testing.T) {
		tests := []struct {
			expires time.Time
			want    string
		}{
			{expires: time.Date(2026, 11, 11, 13, 0, 30, 0, time.FixedZone("", 3600)), want: "2026-11-11T12:00:30.000Z"},
			{expires: time.Date(2026, 11, 11, 12, 0, 30, 120_000_000, time.UTC), want: "2026-11-11T12:00:30.120Z"},
			{expires: time.Date(2026, 11, 11, 12, 0, 30, 123_456_000, time.UTC), want: "2026-11-11T12:00:30.123456Z"},
			{expires: time.Date(2026, 11, 11, 12, 0, 30, 123_456_789, time.UTC), want: "2026-11-11T12:00:30.123456789Z"},
			{expires: time.Date(2026, 11, 11, 12, 0, 30, 1, time.UTC), want: "2026-11-11T12:00:30.000000001Z"},
		}
		for _, test := range tests {
			m := format.Manifest{Lease: &format.Lease{Holder: "h", Expires: test.expires}}
			b, err := m.Marshal()
			is.NotError(t, err)
			is.True(t, bytes.Contains(b, []byte(`"expires": "`+test.want+`"`)), string(b))

			parsed, err := format.ParseManifest(b)
			is.NotError(t, err)
			is.True(t, test.expires.Equal(parsed.Lease.Expires), test.want)
		}
	})

	t.Run("should write characters as they are, without HTML escapes", func(t *testing.T) {
		m := format.Manifest{Lease: &format.Lease{Holder: "<&>æ", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"holder": "<&>`+"æ"+`"`)), string(b))
	})

	t.Run("should escape U+2028 and U+2029", func(t *testing.T) {
		m := format.Manifest{Lease: &format.Lease{Holder: "a\u2028b\u2029c", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"holder": "a\u2028b\u2029c"`)), string(b))
	})

	refused := []struct {
		name  string
		lease format.Lease
	}{
		{name: "should refuse a lease without a holder", lease: format.Lease{Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "should refuse a holder that is not valid UTF-8", lease: format.Lease{Holder: "\xff", Expires: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "should refuse an expiry after year 9999", lease: format.Lease{Holder: "h", Expires: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}},
		{name: "should refuse an expiry before year 0", lease: format.Lease{Holder: "h", Expires: time.Date(-1, 12, 31, 0, 0, 0, 0, time.UTC)}},
		{name: "should refuse an expiry that is after year 9999 only in UTC", lease: format.Lease{Holder: "h", Expires: time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("", -7200))}},
	}
	for _, test := range refused {
		t.Run(test.name, func(t *testing.T) {
			_, err := format.Manifest{Lease: &test.lease}.Marshal()
			is.True(t, errors.Is(err, format.ErrMalformed), "error is not malformed: "+errorString(err))
		})
	}
}

// TestManifestSamples reads the sample manifests in testdata, and checks that they are what the
// current code writes. Run it with -update to write them again.
func TestManifestSamples(t *testing.T) {
	samples := []struct {
		file     string
		manifest format.Manifest
	}{
		{file: "manifest-v0-new.json", manifest: format.Manifest{}},
		{file: "manifest-v0-lease.json", manifest: format.Manifest{
			Head:  format.Name(sha256.Sum256([]byte("sample"))),
			Lease: &format.Lease{Holder: "5b0e3a4f9c1d77c3", Expires: time.Date(2026, 11, 11, 12, 0, 30, 123_000_000, time.UTC)},
		}},
	}

	for _, sample := range samples {
		t.Run("should read "+sample.file+" and write it the same", func(t *testing.T) {
			path := filepath.Join("testdata", sample.file)
			b, err := sample.manifest.Marshal()
			is.NotError(t, err)
			if *update {
				is.NotError(t, os.WriteFile(path, b, 0o644))
			}

			stored, err := os.ReadFile(path)
			is.NotError(t, err)
			is.Equal(t, string(b), string(stored))

			m, err := format.ParseManifest(stored)
			is.NotError(t, err)
			is.Equal(t, sample.manifest.Head, m.Head)
			is.Equal(t, sample.manifest.Lease == nil, m.Lease == nil)
			if m.Lease != nil {
				is.Equal(t, sample.manifest.Lease.Holder, m.Lease.Holder)
				is.True(t, sample.manifest.Lease.Expires.Equal(m.Lease.Expires))
			}
		})
	}

	t.Run("should read manifest-v0-unknown.json and keep every field it does not know", func(t *testing.T) {
		stored, err := os.ReadFile(filepath.Join("testdata", "manifest-v0-unknown.json"))
		is.NotError(t, err)
		m, err := format.ParseManifest(stored)
		is.NotError(t, err)
		b, err := m.Marshal()
		is.NotError(t, err)
		is.Equal(t, string(stored), string(b))
	})
}

func FuzzParseManifest(f *testing.F) {
	for _, file := range []string{"manifest-v0-new.json", "manifest-v0-lease.json", "manifest-v0-unknown.json"} {
		b, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte(`{"version":1}`))
	f.Add(manifestWithLease(`{"holder":"😀","expires":"2026-11-11t13:00:30.5+01:00","x":{"y":[null,true]}}`))
	f.Add(manifestWithLease(`{"holder":"\ud83d\ude00","expires":"2026-11-11t13:00:30.5+01:00","x":{"y":[null,true]}}`))

	f.Fuzz(func(t *testing.T, input []byte) {
		m, err := format.ParseManifest(input)
		if err != nil {
			if !errors.Is(err, format.ErrMalformed) && !errors.Is(err, format.ErrNewerVersion) {
				t.Fatalf("error is neither malformed nor newer version: %v", err)
			}
			return
		}

		written, err := m.Marshal()
		if err != nil {
			t.Fatalf("cannot write a manifest that was read: %v", err)
		}
		again, err := format.ParseManifest(written)
		if err != nil {
			t.Fatalf("cannot read a manifest that was written: %v\n%s", err, written)
		}
		rewritten, err := again.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(written, rewritten) {
			t.Fatalf("written twice, a manifest changed:\n%s\n%s", written, rewritten)
		}

		// Every field is kept: the input and what was written are the same JSON, except for the
		// spelling of the expiry, which is written again in UTC.
		in, out := decodeJSON(t, input), decodeJSON(t, written)
		inLease, outLease := leaseOf(in), leaseOf(out)
		if (inLease == nil) != (outLease == nil) {
			t.Fatalf("lease was lost or added:\n%s\n%s", input, written)
		}
		if inLease != nil {
			inExpires, outExpires := inLease["expires"], outLease["expires"]
			delete(inLease, "expires")
			delete(outLease, "expires")
			if !m.Lease.Expires.Equal(again.Lease.Expires) {
				t.Fatalf("expiry changed from %v to %v", inExpires, outExpires)
			}
		}
		if !reflect.DeepEqual(in, out) {
			t.Fatalf("a field changed:\n%s\n%s", input, written)
		}
	})
}

func manifestWithLease(lease string) []byte {
	return []byte(`{"version":0,"branches":{"main":{"head":"` + strings.Repeat("0", 64) + `"}},"lease":` + lease + `}`)
}

// decodeJSON into maps, slices, and values, with each number as its text, so that numbers are
// compared exactly, and not as float64.
func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	type jsonNumber string
	numbers := json.UnmarshalFromFunc(func(d *jsontext.Decoder, v *any) error {
		if d.PeekKind() != jsontext.KindNumber {
			return errors.ErrUnsupported
		}
		number, err := d.ReadValue()
		*v = jsonNumber(number)
		return err
	})
	var v map[string]any
	if err := json.Unmarshal(b, &v, json.WithUnmarshalers(numbers)); err != nil {
		t.Fatal(err)
	}
	return v
}

func leaseOf(m map[string]any) map[string]any {
	lease, _ := m["lease"].(map[string]any)
	return lease
}

func errorString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}
