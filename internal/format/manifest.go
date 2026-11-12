package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
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
	unknown, unknownBranches, unknownMain map[string]json.RawMessage
}

// Lease as the manifest records it.
type Lease struct {
	// Holder is the ID of the process that holds the lease. It is not empty.
	Holder string
	// Expires is the wall-clock time at which the lease expires. It is written in UTC, and its year
	// in UTC is between 0 and 9999.
	Expires time.Time

	// unknown fields of the lease, by key, as JSON.
	unknown map[string]json.RawMessage
}

// ParseManifest from JSON. It refuses, with an error that wraps [ErrNewerVersion], a version other
// than 0, and with one that wraps [ErrMalformed]:
//
//   - bytes that are not one JSON object, or that are not valid UTF-8, or that hold an escaped
//     UTF-16 surrogate that is not part of a pair
//   - a key given twice in the same object
//   - a missing version, or one that is not a non-negative integer written without a fraction or
//     an exponent
//   - missing branches, or a missing main branch
//   - a head that is not 64 lowercase hexadecimal digits
//   - a lease without a holder, or without an expiry in RFC 3339 format whose year in UTC is between
//     0 and 9999
//
// Keys are matched exactly, so "Version" is a field it does not know, and "version" is then missing.
// A leap second cannot be read.
func ParseManifest(b []byte) (Manifest, error) {
	if !utf8.Valid(b) {
		return Manifest{}, fmt.Errorf("%w: manifest is not valid UTF-8", ErrMalformed)
	}
	if err := checkSurrogates(b); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrMalformed, err)
	}

	top, err := decodeObject(b)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrMalformed, err)
	}

	// The version comes first, because a newer version can change anything else.
	version, ok := top["version"]
	if !ok {
		return Manifest{}, fmt.Errorf("%w: manifest has no version", ErrMalformed)
	}
	delete(top, "version")
	if err := checkManifestVersion(version); err != nil {
		return Manifest{}, err
	}

	var m Manifest

	rawBranches, ok := top["branches"]
	if !ok {
		return Manifest{}, fmt.Errorf("%w: manifest has no branches", ErrMalformed)
	}
	delete(top, "branches")
	branches, err := decodeObject(rawBranches)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest branches: %v", ErrMalformed, err)
	}

	rawMain, ok := branches["main"]
	if !ok {
		return Manifest{}, fmt.Errorf("%w: manifest has no branch main", ErrMalformed)
	}
	delete(branches, "main")
	main, err := decodeObject(rawMain)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest branch main: %v", ErrMalformed, err)
	}

	rawHead, ok := main["head"]
	if !ok {
		return Manifest{}, fmt.Errorf("%w: manifest branch main has no head", ErrMalformed)
	}
	delete(main, "head")
	var head string
	if err := decodeString(rawHead, &head); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest head: %v", ErrMalformed, err)
	}
	if m.Head, err = ParseName(head); err != nil {
		return Manifest{}, fmt.Errorf("manifest head: %w", err)
	}

	if rawLease, ok := top["lease"]; ok {
		delete(top, "lease")
		lease, err := parseLease(rawLease)
		if err != nil {
			return Manifest{}, err
		}
		m.Lease = &lease
	}

	m.unknown, m.unknownBranches, m.unknownMain = top, branches, main
	return m, nil
}

// checkManifestVersion is the number 0, and a version above it wraps [ErrNewerVersion].
func checkManifestVersion(raw json.RawMessage) error {
	s := string(raw)
	if s == fmt.Sprint(manifestVersion) {
		return nil
	}
	if s != "" && s[0] >= '1' && s[0] <= '9' && strings.Trim(s, "0123456789") == "" {
		return fmt.Errorf("%w: manifest version %v, and this version of s4ql reads version %v", ErrNewerVersion, s, manifestVersion)
	}
	return fmt.Errorf("%w: manifest version %v is not a non-negative integer", ErrMalformed, s)
}

func parseLease(raw json.RawMessage) (Lease, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: manifest lease: %v", ErrMalformed, err)
	}

	var lease Lease

	rawHolder, ok := fields["holder"]
	if !ok {
		return Lease{}, fmt.Errorf("%w: manifest lease has no holder", ErrMalformed)
	}
	delete(fields, "holder")
	if err := decodeString(rawHolder, &lease.Holder); err != nil {
		return Lease{}, fmt.Errorf("%w: manifest lease holder: %v", ErrMalformed, err)
	}
	if lease.Holder == "" {
		return Lease{}, fmt.Errorf("%w: manifest lease holder is empty", ErrMalformed)
	}

	rawExpires, ok := fields["expires"]
	if !ok {
		return Lease{}, fmt.Errorf("%w: manifest lease has no expiry", ErrMalformed)
	}
	delete(fields, "expires")
	var expires string
	if err := decodeString(rawExpires, &expires); err != nil {
		return Lease{}, fmt.Errorf("%w: manifest lease expiry: %v", ErrMalformed, err)
	}
	// RFC 3339 allows a lowercase T and Z, and Go's parser does not. No other letter can appear.
	lease.Expires, err = time.Parse(time.RFC3339Nano, strings.ToUpper(expires))
	if err != nil {
		return Lease{}, fmt.Errorf("%w: manifest lease expiry %q is not an RFC 3339 time", ErrMalformed, expires)
	}
	if err := checkExpiry(lease.Expires); err != nil {
		return Lease{}, err
	}

	lease.unknown = fields
	return lease, nil
}

// checkExpiry can be written in RFC 3339 format in UTC, whose years have four digits.
func checkExpiry(t time.Time) error {
	if year := t.UTC().Year(); year < 0 || year > 9999 {
		return fmt.Errorf("%w: manifest lease expiry %v is not between the years 0 and 9999 in UTC", ErrMalformed, t)
	}
	return nil
}

// Marshal the manifest as indented JSON, with the fields it does not know after the known ones, in
// the order of their keys. It refuses, with an error that wraps [ErrMalformed], what
// [ParseManifest] refuses.
func (m Manifest) Marshal() ([]byte, error) {
	head, err := json.Marshal(m.Head)
	if err != nil {
		return nil, err
	}

	main, err := appendObject(nil, []member{{key: "head", value: head}}, m.unknownMain)
	if err != nil {
		return nil, err
	}
	branches, err := appendObject(nil, []member{{key: "main", value: main}}, m.unknownBranches)
	if err != nil {
		return nil, err
	}

	members := []member{
		{key: "version", value: []byte(fmt.Sprint(manifestVersion))},
		{key: "branches", value: branches},
	}

	if m.Lease != nil {
		if m.Lease.Holder == "" {
			return nil, fmt.Errorf("%w: manifest lease has no holder", ErrMalformed)
		}
		if !utf8.ValidString(m.Lease.Holder) {
			return nil, fmt.Errorf("%w: manifest lease holder is not valid UTF-8", ErrMalformed)
		}
		if err := checkExpiry(m.Lease.Expires); err != nil {
			return nil, err
		}
		lease, err := appendObject(nil, []member{
			{key: "holder", value: appendString(nil, m.Lease.Holder)},
			{key: "expires", value: appendString(nil, formatExpiry(m.Lease.Expires))},
		}, m.Lease.unknown)
		if err != nil {
			return nil, err
		}
		members = append(members, member{key: "lease", value: lease})
	}

	compact, err := appendObject(nil, members, m.unknown)
	if err != nil {
		return nil, err
	}

	var b bytes.Buffer
	if err := json.Indent(&b, compact, "", "  "); err != nil {
		return nil, err
	}
	b.WriteByte('\n')
	return b.Bytes(), nil
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

// member of a JSON object, with its value as JSON.
type member struct {
	key   string
	value []byte
}

// appendObject of the known members in their order, then the unknown members in the order of their
// keys, as compact JSON.
func appendObject(b []byte, known []member, unknown map[string]json.RawMessage) ([]byte, error) {
	b = append(b, '{')
	write := func(key string, value []byte) {
		if len(b) > 1 {
			b = append(b, ',')
		}
		b = appendString(b, key)
		b = append(b, ':')
		b = append(b, value...)
	}

	for _, m := range known {
		write(m.key, m.value)
	}

	keys := make([]string, 0, len(unknown))
	for key := range unknown {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		var compact bytes.Buffer
		if err := json.Compact(&compact, unknown[key]); err != nil {
			return nil, err
		}
		write(key, compact.Bytes())
	}

	return append(b, '}'), nil
}

// appendString as a JSON string, without the escapes for HTML that [json.Marshal] adds.
func appendString(b []byte, s string) []byte {
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = e.Encode(s)
	return append(b, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
}

// decodeObject that is the whole of b, by key, and refuse a key given twice.
func decodeObject(b []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()

	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, errors.New("not an object")
	}

	fields := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("key %v is not a string", t)
		}
		if _, ok := fields[key]; ok {
			return nil, fmt.Errorf("key %q is given twice", key)
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}

	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the object")
	}
	return fields, nil
}

// decodeString from b, which must be a JSON string.
func decodeString(b json.RawMessage, s *string) error {
	if len(b) == 0 || b[0] != '"' {
		return fmt.Errorf("%s is not a string", b)
	}
	return json.Unmarshal(b, s)
}

// checkSurrogates in the escapes of the JSON in b, and refuse one that is not part of a pair:
// [json.Unmarshal] replaces it with U+FFFD, and the manifest would change when written again.
func checkSurrogates(b []byte) error {
	inString := false
	expectLow := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}

		var r rune = -1
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 < len(b) && b[i+1] == 'u' && i+5 < len(b) {
				var v rune
				for _, h := range b[i+2 : i+6] {
					switch {
					case h >= '0' && h <= '9':
						v = v<<4 | rune(h-'0')
					case h >= 'a' && h <= 'f':
						v = v<<4 | rune(h-'a'+10)
					case h >= 'A' && h <= 'F':
						v = v<<4 | rune(h-'A'+10)
					default:
						// Not a valid escape. The JSON decoder refuses it.
						return nil
					}
				}
				r = v
				i += 5
			} else {
				// Any other escape is two bytes.
				i++
			}
		}

		isHigh, isLow := r >= 0xd800 && r <= 0xdbff, r >= 0xdc00 && r <= 0xdfff
		if expectLow != isLow {
			return errors.New("escaped UTF-16 surrogate that is not part of a pair")
		}
		expectLow = isHigh
	}
	// A high surrogate at the very end is inside a string that is not closed, which the JSON decoder
	// refuses.
	return nil
}
