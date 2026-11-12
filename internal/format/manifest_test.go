package format_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"math"
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
			"lease": {"holder": "5b0e77c3", "duration": "30s", "renewed": "2026-11-12T12:00:30.123Z", "salt": "c41d07a9e2b35f68"}
		}`))
		is.NotError(t, err)
		is.Equal(t, testHead, m.Head.String())
		is.NotNil(t, m.Lease)
		is.Equal(t, "5b0e77c3", m.Lease.Holder)
		is.Equal(t, 30*time.Second, m.Lease.Duration)
		is.True(t, time.Date(2026, 11, 12, 12, 0, 30, 123_000_000, time.UTC).Equal(m.Lease.Renewed))
		is.Equal(t, uint64(0xc41d07a9e2b35f68), m.Lease.Salt)
	})

	t.Run("should read every duration in the spelling of time.Duration.String", func(t *testing.T) {
		tests := []struct {
			duration string
			want     time.Duration
		}{
			{duration: "1ns", want: time.Nanosecond},
			{duration: "1µs", want: time.Microsecond},
			{duration: "500ms", want: 500 * time.Millisecond},
			{duration: "1s", want: time.Second},
			{duration: "1.5s", want: 1500 * time.Millisecond},
			{duration: "30s", want: 30 * time.Second},
			{duration: "1m0s", want: time.Minute},
			{duration: "1m30s", want: 90 * time.Second},
			{duration: "1h0m0s", want: time.Hour},
			{duration: "2562047h47m16.854775807s", want: time.Duration(math.MaxInt64)},
		}
		for _, test := range tests {
			is.Equal(t, test.duration, test.want.String())
			m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"` + test.duration + `","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))
			is.NotError(t, err, test.duration)
			is.Equal(t, test.want, m.Lease.Duration, test.duration)
		}
	})

	t.Run("should read any 16 lowercase hexadecimal digits as the salt", func(t *testing.T) {
		tests := []struct {
			salt string
			want uint64
		}{
			{salt: "0000000000000000", want: 0},
			{salt: "0000000000000001", want: 1},
			{salt: "0123456789abcdef", want: 0x0123456789abcdef},
			{salt: "ffffffffffffffff", want: math.MaxUint64},
		}
		for _, test := range tests {
			m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"` + test.salt + `"}`))
			is.NotError(t, err, test.salt)
			is.Equal(t, test.want, m.Lease.Salt, test.salt)
		}
	})

	t.Run("should read a manifest without a lease", func(t *testing.T) {
		m, err := format.ParseManifest([]byte(`{"version":0,"branches":{"main":{"head":"` + strings.Repeat("0", 64) + `"}}}`))
		is.NotError(t, err)
		is.Equal(t, format.EmptyLog, m.Head)
		is.True(t, m.Lease == nil, "lease is not nil")
	})

	t.Run("should read any RFC 3339 time as the renewal time", func(t *testing.T) {
		want := time.Date(2026, 11, 11, 12, 0, 30, 0, time.UTC)
		for _, renewed := range []string{
			"2026-11-11T12:00:30Z",
			"2026-11-11T12:00:30.000000000Z",
			"2026-11-11T13:00:30+01:00",
			"2026-11-11T11:30:30-00:30",
			"2026-11-11t12:00:30z",
		} {
			m, err := format.ParseManifest(manifestWithLease(`{"holder":"a","duration":"30s","renewed":"` + renewed + `","salt":"c41d07a9e2b35f68"}`))
			is.NotError(t, err, renewed)
			is.True(t, want.Equal(m.Lease.Renewed), renewed)
		}
	})

	// This test sets time.Local, so it must not run in parallel.
	t.Run("should return the renewal time in UTC and write the same error text whatever the time zone of the machine", func(t *testing.T) {
		local := time.Local
		t.Cleanup(func() { time.Local = local })

		var errTexts []string
		for _, zone := range []*time.Location{time.UTC, time.FixedZone("TEST", 3600), time.FixedZone("OTHER", -7200)} {
			time.Local = zone

			// The offset +01:00 is the one of the second zone, for which time.Parse returns time.Local.
			for _, renewed := range []string{"2026-11-11T13:00:30+01:00", "2026-11-11T12:00:30Z", "2026-11-11T05:30:30-06:30"} {
				m, err := format.ParseManifest(manifestWithLease(`{"holder":"a","duration":"30s","renewed":"` + renewed + `","salt":"c41d07a9e2b35f68"}`))
				is.NotError(t, err, renewed)
				is.True(t, time.Date(2026, 11, 11, 12, 0, 30, 0, time.UTC).Equal(m.Lease.Renewed), renewed)
				is.True(t, m.Lease.Renewed.Location() == time.UTC, renewed+" in "+zone.String())
			}

			_, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"0000-01-01T00:00:00+01:00","salt":"c41d07a9e2b35f68"}`))
			is.True(t, errors.Is(err, format.ErrMalformed), "error is not ErrMalformed")
			errTexts = append(errTexts, err.Error())
		}
		is.Equal(t, errTexts[0], errTexts[1])
		is.Equal(t, errTexts[0], errTexts[2])
	})

	t.Run("should read a renewal time with more than nine digits of fraction to the nanosecond", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30.1234567899Z","salt":"c41d07a9e2b35f68"}`))
		is.NotError(t, err)
		is.True(t, time.Date(2026, 11, 11, 12, 0, 30, 123_456_789, time.UTC).Equal(m.Lease.Renewed))
	})

	t.Run("should keep an expiry as a field it does not know when the lease has a renewal time", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68","expires":"2026-11-11T12:00:30Z"}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"salt": "c41d07a9e2b35f68",
    "expires": "2026-11-11T12:00:30Z"
  }`)), string(b))
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
			"lease": {"salt": "c41d07a9e2b35f68", "holder": "h", "epoch": 7, "renewed": "2026-11-12T12:00:30.123Z", "duration": "30s"}
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
    "duration": "30s",
    "renewed": "2026-11-12T12:00:30.123Z",
    "salt": "c41d07a9e2b35f68",
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
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68","x":["\u0041\/<&>",1.50,1E+2,-0]}`))
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
			`"lease":{"holder":"h",` + unknown + `,"duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		v := jsontext.Value(b)
		is.NotError(t, v.Compact())
		sorted := `"B":0,"_":0,"a":0,"b":0,"é":0`
		is.Equal(t, `{"version":0,"branches":{"main":{"head":"`+testHead+`",`+sorted+`},`+sorted+`},`+
			`"lease":{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30.000Z","salt":"c41d07a9e2b35f68",`+sorted+`},`+sorted+`}`, string(v))
	})

	t.Run("should escape U+2028 and U+2029 in the fields it does not know, and keep the rest of their strings", func(t *testing.T) {
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68","x":"a` + "\u2028" + `b` + "\u2029" + `c\/"}`))
		is.NotError(t, err)

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"x": "a\u2028b\u2029c\/"`)), string(b))
	})

	t.Run("should not change when the bytes it was read from change", func(t *testing.T) {
		input := []byte(`{"version":0,"x":"abc","branches":{"main":{"head":"` + testHead + `","x":"abc"},"x":"abc"},` +
			`"lease":{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68","x":"abc"}}`)
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
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68","epoch":7}`))
		is.NotError(t, err)
		m.Head = format.Name(sha256.Sum256([]byte("next")))
		m.Lease.Duration = time.Minute
		m.Lease.Renewed = m.Lease.Renewed.Add(time.Second)
		m.Lease.Salt = 0x0123456789abcdef

		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"epoch": 7`)), "epoch is gone")
		is.True(t, bytes.Contains(b, []byte(`"duration": "1m0s"`)), "duration is wrong")
		is.True(t, bytes.Contains(b, []byte(`"renewed": "2026-11-12T12:00:31.000Z"`)), "renewal time is wrong")
		is.True(t, bytes.Contains(b, []byte(`"salt": "0123456789abcdef"`)), "salt is wrong")
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
		{name: "should refuse a lease without a holder", input: string(manifestWithLease(`{"duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease with an empty holder", input: string(manifestWithLease(`{"holder":"","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease whose holder is not a string", input: string(manifestWithLease(`{"holder":1,"duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease without a duration", input: string(manifestWithLease(`{"holder":"h","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease without a renewal time", input: string(manifestWithLease(`{"holder":"h","duration":"30s","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease without a salt", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z"}`))},
		{name: "should refuse a lease with an expiry and no renewal time", input: string(manifestWithLease(`{"holder":"h","duration":"30s","expires":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease of the old form with only a holder and an expiry", input: string(manifestWithLease(`{"holder":"h","expires":"2026-11-12T12:00:30Z"}`))},
		{name: "should refuse a lease whose duration is null", input: string(manifestWithLease(`{"holder":"h","duration":null,"renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease whose renewal time is null", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":null,"salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease whose salt is null", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":null}`))},
		{name: "should refuse a duration that is a number", input: string(manifestWithLease(`{"holder":"h","duration":30,"renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration that is an empty string", input: string(manifestWithLease(`{"holder":"h","duration":"","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration of zero", input: string(manifestWithLease(`{"holder":"h","duration":"0s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration of zero without a unit", input: string(manifestWithLease(`{"holder":"h","duration":"0","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a negative duration", input: string(manifestWithLease(`{"holder":"h","duration":"-30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with a plus sign", input: string(manifestWithLease(`{"holder":"h","duration":"+30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with a fraction of a larger unit", input: string(manifestWithLease(`{"holder":"h","duration":"0.5m","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration in a smaller unit than its canonical spelling", input: string(manifestWithLease(`{"holder":"h","duration":"30000ms","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with a unit of zero after it", input: string(manifestWithLease(`{"holder":"h","duration":"30s0ms","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration of whole minutes without its seconds", input: string(manifestWithLease(`{"holder":"h","duration":"1m","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration of more than 59 seconds", input: string(manifestWithLease(`{"holder":"h","duration":"90s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with a trailing zero in its fraction", input: string(manifestWithLease(`{"holder":"h","duration":"1.50s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with a leading zero", input: string(manifestWithLease(`{"holder":"h","duration":"030s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration in microseconds written with us", input: string(manifestWithLease(`{"holder":"h","duration":"1us","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration in microseconds written with the Greek letter mu", input: string(manifestWithLease(`{"holder":"h","duration":"1μs","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with spaces", input: string(manifestWithLease(`{"holder":"h","duration":" 30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with an uppercase unit", input: string(manifestWithLease(`{"holder":"h","duration":"30S","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration without a unit", input: string(manifestWithLease(`{"holder":"h","duration":"30","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration with an exponent", input: string(manifestWithLease(`{"holder":"h","duration":"3e1s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a duration that overflows", input: string(manifestWithLease(`{"holder":"h","duration":"2562047h47m16.854775808s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a salt that is a number", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":1234567890123456}`))},
		{name: "should refuse a salt that is an empty string", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":""}`))},
		{name: "should refuse a salt of 15 digits", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f6"}`))},
		{name: "should refuse a salt of 17 digits", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f680"}`))},
		{name: "should refuse a salt with uppercase digits", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"C41D07A9E2B35F68"}`))},
		{name: "should refuse a salt with a digit that is not hexadecimal", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f6g"}`))},
		{name: "should refuse a salt with a prefix of 0x", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"0xc41d07a9e2b35f"}`))},
		{name: "should refuse a salt with a sign", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"+c41d07a9e2b35f6"}`))},
		{name: "should refuse a salt with an underscore", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9_2b35f68"}`))},
		{name: "should refuse a salt with a space", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":" c41d07a9e2b35f6"}`))},
		{name: "should refuse a lease whose renewal time is not a string", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":1,"salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time without a time zone", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time that is only a date", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with a comma before the fraction", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30,123Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with a dot and no fraction", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30.Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with an offset of 24 hours", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30+24:00","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with an offset of 60 minutes", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30+23:60","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with an offset without a colon", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T12:00:30+0100","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with a month of one digit", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-1-11T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time at hour 24", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11T24:00:00Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with a leap second", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-12-31T23:59:60Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time with a space for the T", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-11-11 12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time on a day that does not exist", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"2026-02-30T12:00:30Z","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time before year 0 in UTC", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"0000-01-01T00:00:00+01:00","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a renewal time after year 9999 in UTC", input: string(manifestWithLease(`{"holder":"h","duration":"30s","renewed":"9999-12-31T23:59:59-01:00","salt":"c41d07a9e2b35f68"}`))},
		{name: "should refuse a lease given twice", input: `{"version":0,"branches":{"main":{"head":"` + zeros + `"}},"lease":{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"},"lease":{"holder":"h","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}}`},
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
		m, err := format.ParseManifest(manifestWithLease(`{"holder":"\ud83d\ude00","duration":"30s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))
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

	t.Run("should write the lease with its holder, duration, renewal time, and salt, in that order", func(t *testing.T) {
		m := format.Manifest{Lease: &format.Lease{
			Holder:   "5b0e77c3",
			Duration: 30 * time.Second,
			Renewed:  time.Date(2026, 11, 12, 12, 0, 30, 123_456_789, time.UTC),
			Salt:     0xc41d07a9e2b35f68,
		}}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`
  "lease": {
    "holder": "5b0e77c3",
    "duration": "30s",
    "renewed": "2026-11-12T12:00:30.123456789Z",
    "salt": "c41d07a9e2b35f68"
  }
`)), string(b))
	})

	t.Run("should write the duration as time.Duration.String does", func(t *testing.T) {
		for _, d := range []time.Duration{
			time.Nanosecond, time.Microsecond, 500 * time.Millisecond, time.Second, 1500 * time.Millisecond,
			30 * time.Second, time.Minute, 90 * time.Second, time.Hour, 36*time.Hour + time.Nanosecond, math.MaxInt64,
		} {
			m := format.Manifest{Lease: &format.Lease{Holder: "h", Duration: d, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
			b, err := m.Marshal()
			is.NotError(t, err, d.String())
			is.True(t, bytes.Contains(b, []byte(`"duration": "`+d.String()+`"`)), string(b))

			parsed, err := format.ParseManifest(b)
			is.NotError(t, err, d.String())
			is.Equal(t, d, parsed.Lease.Duration)
		}
	})

	t.Run("should write the salt as 16 lowercase hexadecimal digits", func(t *testing.T) {
		tests := []struct {
			salt uint64
			want string
		}{
			{salt: 0, want: "0000000000000000"},
			{salt: 1, want: "0000000000000001"},
			{salt: 0xabcdef, want: "0000000000abcdef"},
			{salt: 0xc41d07a9e2b35f68, want: "c41d07a9e2b35f68"},
			{salt: math.MaxUint64, want: "ffffffffffffffff"},
		}
		for _, test := range tests {
			m := format.Manifest{Lease: &format.Lease{Holder: "h", Duration: time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: test.salt}}
			b, err := m.Marshal()
			is.NotError(t, err)
			is.True(t, bytes.Contains(b, []byte(`"salt": "`+test.want+`"`)), string(b))

			parsed, err := format.ParseManifest(b)
			is.NotError(t, err)
			is.Equal(t, test.salt, parsed.Lease.Salt)
		}
	})

	t.Run("should write the renewal time in UTC with fractions of a second", func(t *testing.T) {
		tests := []struct {
			renewed time.Time
			want    string
		}{
			{renewed: time.Date(2026, 11, 11, 13, 0, 30, 0, time.FixedZone("", 3600)), want: "2026-11-11T12:00:30.000Z"},
			{renewed: time.Date(2026, 11, 11, 12, 0, 30, 120_000_000, time.UTC), want: "2026-11-11T12:00:30.120Z"},
			{renewed: time.Date(2026, 11, 11, 12, 0, 30, 123_456_000, time.UTC), want: "2026-11-11T12:00:30.123456Z"},
			{renewed: time.Date(2026, 11, 11, 12, 0, 30, 123_456_789, time.UTC), want: "2026-11-11T12:00:30.123456789Z"},
			{renewed: time.Date(2026, 11, 11, 12, 0, 30, 1, time.UTC), want: "2026-11-11T12:00:30.000000001Z"},
		}
		for _, test := range tests {
			m := format.Manifest{Lease: &format.Lease{Holder: "h", Duration: 30 * time.Second, Renewed: test.renewed, Salt: 0xc41d07a9e2b35f68}}
			b, err := m.Marshal()
			is.NotError(t, err)
			is.True(t, bytes.Contains(b, []byte(`"renewed": "`+test.want+`"`)), string(b))

			parsed, err := format.ParseManifest(b)
			is.NotError(t, err)
			is.True(t, test.renewed.Equal(parsed.Lease.Renewed), test.want)
		}
	})

	t.Run("should write characters as they are, without HTML escapes", func(t *testing.T) {
		m := format.Manifest{Lease: &format.Lease{Holder: "<&>æ", Duration: 30 * time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"holder": "<&>`+"æ"+`"`)), string(b))
	})

	t.Run("should escape U+2028 and U+2029", func(t *testing.T) {
		m := format.Manifest{Lease: &format.Lease{Holder: "a\u2028b\u2029c", Duration: 30 * time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}}
		b, err := m.Marshal()
		is.NotError(t, err)
		is.True(t, bytes.Contains(b, []byte(`"holder": "a\u2028b\u2029c"`)), string(b))
	})

	refused := []struct {
		name  string
		lease format.Lease
	}{
		{name: "should refuse a lease without a holder", lease: format.Lease{Duration: 30 * time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a holder that is not valid UTF-8", lease: format.Lease{Holder: "\xff", Duration: 30 * time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a lease without a duration", lease: format.Lease{Holder: "h", Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a negative duration", lease: format.Lease{Holder: "h", Duration: -time.Second, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse the smallest duration", lease: format.Lease{Holder: "h", Duration: math.MinInt64, Renewed: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a renewal time after year 9999", lease: format.Lease{Holder: "h", Duration: 30 * time.Second, Renewed: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a renewal time before year 0", lease: format.Lease{Holder: "h", Duration: 30 * time.Second, Renewed: time.Date(-1, 12, 31, 0, 0, 0, 0, time.UTC), Salt: 0xc41d07a9e2b35f68}},
		{name: "should refuse a renewal time that is after year 9999 only in UTC", lease: format.Lease{Holder: "h", Duration: 30 * time.Second, Renewed: time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("", -7200)), Salt: 0xc41d07a9e2b35f68}},
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
			Lease: &format.Lease{
				Holder:   "5b0e3a4f9c1d77c3",
				Duration: 30 * time.Second,
				Renewed:  time.Date(2026, 11, 12, 12, 0, 30, 123_000_000, time.UTC),
				Salt:     0xc41d07a9e2b35f68,
			},
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
				is.Equal(t, sample.manifest.Lease.Duration, m.Lease.Duration)
				is.True(t, sample.manifest.Lease.Renewed.Equal(m.Lease.Renewed))
				is.Equal(t, sample.manifest.Lease.Salt, m.Lease.Salt)
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
	f.Add(manifestWithLease(`{"holder":"😀","duration":"1m30s","renewed":"2026-11-12t13:00:30.5+01:00","salt":"0123456789abcdef","x":{"y":[null,true]}}`))
	f.Add(manifestWithLease(`{"holder":"\ud83d\ude00","duration":"1m30s","renewed":"2026-11-12t13:00:30.5+01:00","salt":"0123456789abcdef","x":{"y":[null,true]}}`))
	f.Add(manifestWithLease(`{"holder":"h","duration":"1h0m0.000000001s","renewed":"2026-11-12T12:00:30Z","salt":"ffffffffffffffff","expires":"2026-11-11T12:00:30Z"}`))
	f.Add(manifestWithLease(`{"holder":"h","duration":"90s","renewed":"2026-11-12T12:00:30Z","salt":"c41d07a9e2b35f68"}`))

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
		// spelling of the renewal time, which is written again in UTC. The duration and the salt each
		// have one spelling, so they are written as they were read.
		in, out := decodeJSON(t, input), decodeJSON(t, written)
		inLease, outLease := leaseOf(in), leaseOf(out)
		if (inLease == nil) != (outLease == nil) {
			t.Fatalf("lease was lost or added:\n%s\n%s", input, written)
		}
		if inLease != nil {
			if m.Lease.Duration <= 0 {
				t.Fatalf("read a duration of %v", m.Lease.Duration)
			}
			if m.Lease.Duration != again.Lease.Duration || m.Lease.Salt != again.Lease.Salt {
				t.Fatalf("duration or salt changed:\n%s\n%s", input, written)
			}
			inRenewed, outRenewed := inLease["renewed"], outLease["renewed"]
			delete(inLease, "renewed")
			delete(outLease, "renewed")
			if !m.Lease.Renewed.Equal(again.Lease.Renewed) {
				t.Fatalf("renewal time changed from %v to %v", inRenewed, outRenewed)
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
