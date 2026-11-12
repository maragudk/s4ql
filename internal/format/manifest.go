package format

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strconv"
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
//   - "lease", which is optional: an object with the "holder", a non-empty string; the "duration",
//     a positive duration in the spelling of [time.Duration.String], such as "30s" or "1m30s";
//     "renewed", a time in RFC 3339 format; and the "salt", 16 lowercase hexadecimal digits. It is
//     missing when no instance holds the lease.
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

// Lease as the manifest records it. The package reads no clock and draws no random numbers, so the
// holder sets the renewal time and the salt at each write of the manifest.
type Lease struct {
	// Holder is the ID of the process that holds the lease. It is not empty.
	Holder string
	// Duration of the lease. It is above zero, and is written as [time.Duration.String] writes it.
	Duration time.Duration
	// Renewed is the holder's wall-clock time at the renewal. It is written in UTC, and its year in
	// UTC is between 0 and 9999.
	Renewed time.Time
	// Salt is 64 random bits, new at each write of the manifest by the holder, so that each write
	// changes the bytes of the manifest. It is written as 16 lowercase hexadecimal digits.
	Salt uint64

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

// leaseJSON holds the duration, the renewal time, and the salt as strings, because the
// spellings that the manifest accepts are narrower than those of the decoders for [time.Duration],
// [time.Time], and uint64, and are checked by [parseLease].
type leaseJSON struct {
	Holder   string                    `json:"holder"`
	Duration string                    `json:"duration"`
	Renewed  string                    `json:"renewed"`
	Salt     string                    `json:"salt"`
	Unknown  map[string]jsontext.Value `json:",embed"`
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
//   - a lease without a holder, without a duration, without a renewal time, or without a salt
//   - a duration that is not a string, that is not above zero, or that is not spelled exactly as
//     [time.Duration.String] spells it, such as "90s" for "1m30s", "30000ms" for "30s", or "+30s"
//   - a renewal time that is not in RFC 3339 format, or whose year in UTC is not between 0 and 9999
//   - a salt that is not exactly 16 lowercase hexadecimal digits
//
// The first two points are checked before the version, at every level, so bytes that fail them are
// malformed whatever their version. Keys are matched exactly, so
// "Version" is a field it does not know, and "version" is then missing. "expires" is a field it
// does not know too, so a lease with "expires" and without one of the four fields is refused like
// any lease that lacks a field.
// A leap second cannot be read, and a renewal time is read to the nanosecond, with more digits
// dropped. The renewal time that it returns is in UTC, whatever its offset and the time zone of the
// machine.
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
	// A missing or null field is read as the empty string, and refused below.
	if j.Holder == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no holder", ErrMalformed)
	}
	if j.Duration == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no duration", ErrMalformed)
	}
	if j.Renewed == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no renewal time", ErrMalformed)
	}
	if j.Salt == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease has no salt", ErrMalformed)
	}

	d, err := parseDuration(j.Duration)
	if err != nil {
		return Lease{}, err
	}
	renewed, err := parseRenewed(j.Renewed)
	if err != nil {
		return Lease{}, err
	}
	salt, err := parseSalt(j.Salt)
	if err != nil {
		return Lease{}, err
	}

	return Lease{Holder: j.Holder, Duration: d, Renewed: renewed, Salt: salt, unknown: j.Unknown}, nil
}

// parseDuration in the spelling of [time.Duration.String], and nothing else, so that a duration
// has one spelling.
func parseDuration(s string) (time.Duration, error) {
	// time.ParseDuration refuses a duration beyond the range of time.Duration, and reads many
	// spellings of each duration that it accepts. Of those, only the one that String writes is
	// accepted.
	d, err := time.ParseDuration(s)
	if err != nil || d.String() != s {
		return 0, fmt.Errorf("%w: manifest lease duration %q is not a duration as Go writes it", ErrMalformed, s)
	}
	if err := checkDuration(d); err != nil {
		return 0, err
	}
	return d, nil
}

// checkDuration is above zero.
func checkDuration(d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("%w: manifest lease duration %v is not above zero", ErrMalformed, d)
	}
	return nil
}

// parseRenewed from RFC 3339 format, and return it in UTC.
func parseRenewed(s string) (time.Time, error) {
	// RFC 3339 allows a lowercase T and Z, and Go's parser does not. No other letter can appear.
	renewed := strings.ToUpper(s)
	// Go's parser also reads a comma before the fraction, and offsets of 24 hours or 60 minutes,
	// which RFC 3339 does not allow, so the syntax is checked first.
	if !rfc3339.MatchString(renewed) {
		return time.Time{}, fmt.Errorf("%w: manifest lease renewal time %q is not an RFC 3339 time", ErrMalformed, renewed)
	}
	t, err := time.Parse(time.RFC3339Nano, renewed)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: manifest lease renewal time %q is not an RFC 3339 time", ErrMalformed, renewed)
	}
	// time.Parse returns a time in time.Local if the offset is the one of the machine, so the
	// location of the result, and the text of an error that prints it, would depend on the machine.
	t = t.UTC()
	if err := checkRenewed(t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// rfc3339 is the syntax of date-time in RFC 3339, section 5.6, with uppercase letters. The parser
// checks the ranges of the date and the time.
var rfc3339 = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

// checkRenewed can be written in RFC 3339 format in UTC, whose years have four digits.
func checkRenewed(t time.Time) error {
	if year := t.UTC().Year(); year < 0 || year > 9999 {
		return fmt.Errorf("%w: manifest lease renewal time %v is not between the years 0 and 9999 in UTC", ErrMalformed, t)
	}
	return nil
}

// parseSalt from exactly 16 lowercase hexadecimal digits.
func parseSalt(s string) (uint64, error) {
	if len(s) != 16 {
		return 0, fmt.Errorf("%w: manifest lease salt %q is not 16 hexadecimal digits", ErrMalformed, s)
	}
	// strconv.ParseUint also reads uppercase digits, so the digits are checked first.
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return 0, fmt.Errorf("%w: manifest lease salt %q is not 16 lowercase hexadecimal digits", ErrMalformed, s)
		}
	}
	salt, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: manifest lease salt %q: %v", ErrMalformed, s, err)
	}
	return salt, nil
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
		if err := checkDuration(m.Lease.Duration); err != nil {
			return nil, err
		}
		if err := checkRenewed(m.Lease.Renewed); err != nil {
			return nil, err
		}
		j.Lease = &leaseJSON{
			Holder:   m.Lease.Holder,
			Duration: m.Lease.Duration.String(),
			Renewed:  formatRenewed(m.Lease.Renewed),
			Salt:     fmt.Sprintf("%016x", m.Lease.Salt),
			Unknown:  m.Lease.unknown,
		}
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

// formatRenewed in UTC in RFC 3339 format, with as many digits of the fraction of a second as it
// needs, in groups of three, and at least three.
func formatRenewed(t time.Time) string {
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
