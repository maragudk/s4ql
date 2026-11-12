package format

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// manifestVersion is the version of the manifest format that this package reads and writes.
const manifestVersion = 0

// Manifest of a database: the object manifest.json under its prefix, which points to the newest
// commit object and records the lease.
//
// The JSON of version 0 is an object with:
//
//   - "version": the number 0.
//   - "branches": an object with the entry "main", an object whose "head" is the [Name] of the
//     newest commit object, or of [EmptyLog] for a new database.
//   - "lease", which is optional: an object with the "holder", a non-empty string, and "expires", a
//     time in RFC 3339 format. It is missing when no instance holds the lease.
//
// A manifest from [ParseManifest] keeps the fields that it does not know, in the manifest itself,
// in "branches", in "main", and in "lease", including branches other than "main", and
// [Manifest.Marshal] writes them again. An older version of s4ql then does not delete the fields of
// a newer one when it rewrites the manifest. Changing [Manifest.Head] or the fields of
// [Manifest.Lease] keeps them; a new [Lease] has none.
//
// The zero Manifest is the manifest of a new database: its head is [EmptyLog], and no instance
// holds its lease.
type Manifest struct {
	// Head of the branch "main": the name of the newest commit object, or [EmptyLog].
	Head Name
	// Lease, or nil if no instance holds it.
	Lease *Lease

	// unknown fields of the manifest itself, of "branches", and of "main", by key, as JSON.
	unknown, unknownBranches, unknownMain map[string]jsontext.Value
}

// Lease as the manifest records it.
type Lease struct {
	// Holder is the ID of the process that holds the lease. It is not empty.
	Holder string
	// Expires is the wall-clock time at which the lease expires. It is written in UTC, and its year
	// in UTC is between 0 and 9999.
	Expires time.Time

	// unknown fields of the lease, by key, as JSON.
	unknown map[string]jsontext.Value
}

// manifestJSON is the JSON of a [Manifest], for [json.Marshal] and [json.Unmarshal]. At each level,
// the field Unknown is an embedded fallback: it holds the members that the level does not know.
type manifestJSON struct {
	Version  jsontext.Value            `json:"version"`
	Branches *branchesJSON             `json:"branches"`
	Lease    *leaseJSON                `json:"lease,omitzero"`
	Unknown  map[string]jsontext.Value `json:",embed"`
}

type branchesJSON struct {
	Main    *branchJSON               `json:"main"`
	Unknown map[string]jsontext.Value `json:",embed"`
}

type branchJSON struct {
	Head    *Name                     `json:"head"`
	Unknown map[string]jsontext.Value `json:",embed"`
}

type leaseJSON struct {
	Holder  string                    `json:"holder"`
	Expires string                    `json:"expires"`
	Unknown map[string]jsontext.Value `json:",embed"`
}

// ParseManifest from JSON. It refuses, with an error that wraps [ErrNewerVersion], a version other
// than 0, and with one that wraps [ErrMalformed]:
//
//   - bytes that are not one JSON object, or that are not valid UTF-8, or that hold an escaped
//     UTF-16 surrogate that is not part of a pair
//   - a key given twice in the same object, at any level
//   - a missing version, or one that is not a non-negative integer written without a fraction or
//     an exponent
//   - missing branches, or a missing main branch
//   - a head that is not 64 lowercase hexadecimal digits
//   - a lease without a holder, or without an expiry in RFC 3339 format whose year in UTC is between
//     0 and 9999
//
// The first two points are checked before the version, at every level, so bytes that fail them are
// malformed whatever their version. Keys are matched exactly, so
// "Version" is a field it does not know, and "version" is then missing. A leap second cannot be
// read, and an expiry is read to the nanosecond, with more digits dropped. The expiry that it
// returns is in UTC, whatever its offset and the time zone of the machine.
func ParseManifest(b []byte) (Manifest, error) {
	// Each [json.Unmarshal] refuses invalid syntax, invalid UTF-8, lone surrogates, and keys given
	// twice, at every level. The second also refuses values of the wrong kind. The rest is checked
	// here.
	//
	// The version is checked before the rest is read, because a newer version can change anything
	// else. The lease is kept as JSON in the first pass, because [manifestJSON] reads a null lease as
	// no lease.
	var first struct {
		Version jsontext.Value `json:"version"`
		Lease   jsontext.Value `json:"lease"`
	}
	if err := json.Unmarshal(b, &first); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrMalformed, err)
	}
	if first.Version == nil {
		return Manifest{}, fmt.Errorf("%w: manifest has no version", ErrMalformed)
	}
	if err := checkManifestVersion(first.Version); err != nil {
		return Manifest{}, err
	}
	if first.Lease.Kind() == jsontext.KindNull {
		return Manifest{}, fmt.Errorf("%w: manifest lease is null", ErrMalformed)
	}

	var j manifestJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrMalformed, err)
	}
	if j.Branches == nil {
		return Manifest{}, fmt.Errorf("%w: manifest has no branches", ErrMalformed)
	}
	if j.Branches.Main == nil {
		return Manifest{}, fmt.Errorf("%w: manifest has no branch main", ErrMalformed)
	}
	if j.Branches.Main.Head == nil {
		return Manifest{}, fmt.Errorf("%w: manifest branch main has no head", ErrMalformed)
	}

	m := Manifest{
		Head:            *j.Branches.Main.Head,
		unknown:         j.Unknown,
		unknownBranches: j.Branches.Unknown,
		unknownMain:     j.Branches.Main.Unknown,
	}
	if j.Lease != nil {
		lease, err := parseLease(*j.Lease)
		if err != nil {
			return Manifest{}, err
		}
		m.Lease = &lease
	}
	return m, nil
}

// checkManifestVersion is the number 0, and a version above it wraps [ErrNewerVersion].
func checkManifestVersion(raw jsontext.Value) error {
	s := string(raw)
	if s == fmt.Sprint(manifestVersion) {
		return nil
	}
	if s != "" && s[0] >= '1' && s[0] <= '9' && strings.Trim(s, "0123456789") == "" {
		return fmt.Errorf("%w: manifest version %v, and this version of s4ql reads version %v", ErrNewerVersion, s, manifestVersion)
	}
	return fmt.Errorf("%w: manifest version %v is not a non-negative integer", ErrMalformed, s)
}

func parseLease(j leaseJSON) (Lease, error) {
	// A missing or null holder or expiry is read as the empty string, and refused below.
	if j.Holder == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no holder", ErrMalformed)
	}
	if j.Expires == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no expiry", ErrMalformed)
	}

	// RFC 3339 allows a lowercase T and Z, and Go's parser does not. No other letter can appear.
	expires := strings.ToUpper(j.Expires)
	// Go's parser also reads a comma before the fraction, and offsets of 24 hours or 60 minutes,
	// which RFC 3339 does not allow, so the syntax is checked first.
	if !rfc3339.MatchString(expires) {
		return Lease{}, fmt.Errorf("%w: manifest lease expiry %q is not an RFC 3339 time", ErrMalformed, expires)
	}
	t, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: manifest lease expiry %q is not an RFC 3339 time", ErrMalformed, expires)
	}
	// time.Parse returns a time in time.Local if the offset is the one of the machine, so the
	// location of the result, and the text of an error that prints it, would depend on the machine.
	t = t.UTC()
	if err := checkExpiry(t); err != nil {
		return Lease{}, err
	}

	return Lease{Holder: j.Holder, Expires: t, unknown: j.Unknown}, nil
}

// rfc3339 is the syntax of date-time in RFC 3339, section 5.6, with uppercase letters. The parser
// checks the ranges of the date and the time.
var rfc3339 = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// checkExpiry can be written in RFC 3339 format in UTC, whose years have four digits.
func checkExpiry(t time.Time) error {
	if year := t.UTC().Year(); year < 0 || year > 9999 {
		return fmt.Errorf("%w: manifest lease expiry %v is not between the years 0 and 9999 in UTC", ErrMalformed, t)
	}
	return nil
}

// Marshal the manifest as JSON indented with two spaces and ending in a newline, with the fields it
// does not know after the known ones, in the order of their keys. It refuses, with an error that
// wraps [ErrMalformed], what [ParseManifest] refuses.
//
// U+2028 and U+2029 are escaped in every string, and "<", ">", and "&" are not. The values of the
// fields it does not know are written as they were read, apart from whitespace and those two
// characters, so their strings keep their escapes and their numbers their spelling. The names of
// those fields are written in the shortest form, like the strings of the known fields.
func (m Manifest) Marshal() ([]byte, error) {
	j := manifestJSON{
		Version: jsontext.Value(fmt.Sprint(manifestVersion)),
		Branches: &branchesJSON{
			Main:    &branchJSON{Head: &m.Head, Unknown: m.unknownMain},
			Unknown: m.unknownBranches,
		},
		Unknown: m.unknown,
	}

	if m.Lease != nil {
		if m.Lease.Holder == "" {
			return nil, fmt.Errorf("%w: manifest lease has no holder", ErrMalformed)
		}
		if err := checkExpiry(m.Lease.Expires); err != nil {
			return nil, err
		}
		j.Lease = &leaseJSON{Holder: m.Lease.Holder, Expires: formatExpiry(m.Lease.Expires), Unknown: m.Lease.unknown}
	}

	b, err := json.Marshal(&j,
		jsontext.WithIndent("  "),
		json.Deterministic(true),
		jsontext.PreserveRawStrings(true),
		jsontext.EscapeForJS(true),
	)
	if err != nil {
		// A holder that is not valid UTF-8 is the one way to get here.
		return nil, fmt.Errorf("%w: manifest: %v", ErrMalformed, err)
	}
	return append(b, '\n'), nil
}

// formatExpiry in UTC in RFC 3339 format, with as many digits of the fraction of a second as it
// needs, in groups of three, and at least three.
func formatExpiry(t time.Time) string {
	t = t.UTC()
	nanos := t.Nanosecond()
	var fraction string
	switch {
	case nanos%1_000_000 == 0:
		fraction = fmt.Sprintf("%03d", nanos/1_000_000)
	case nanos%1000 == 0:
		fraction = fmt.Sprintf("%06d", nanos/1000)
	default:
		fraction = fmt.Sprintf("%09d", nanos)
	}
	return t.Format("2006-01-02T15:04:05") + "." + fraction + "Z"
}
