package format

import (
	"encoding/hex"
	"fmt"
)

// Name of a commit object: the SHA-256 checksum of all its bytes. It is the type of the manifest's
// head, of a commit object's parent, and of a commit object's own name. Written as text, it is 64
// lowercase hexadecimal digits.
type Name [32]byte

// EmptyLog is the name of the log before its first commit: 32 zero bytes. It is the head of a new
// database and the parent of its first commit object.
var EmptyLog Name

// ParseName from exactly 64 lowercase hexadecimal digits.
func ParseName(s string) (Name, error) {
	var n Name
	if len(s) != 2*len(n) {
		return Name{}, fmt.Errorf("%w: name %q is not 64 hexadecimal digits", ErrMalformed, s)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return Name{}, fmt.Errorf("%w: name %q is not 64 lowercase hexadecimal digits", ErrMalformed, s)
		}
	}
	if _, err := hex.Decode(n[:], []byte(s)); err != nil {
		return Name{}, fmt.Errorf("%w: name %q: %v", ErrMalformed, s, err)
	}
	return n, nil
}

// NameFromBytes that are exactly 32 bytes long, such as the parent in a commit object's header.
func NameFromBytes(b []byte) (Name, error) {
	var n Name
	if len(b) != len(n) {
		return Name{}, fmt.Errorf("%w: name is %v bytes, not 32", ErrMalformed, len(b))
	}
	copy(n[:], b)
	return n, nil
}

// String of 64 lowercase hexadecimal digits.
func (n Name) String() string {
	return hex.EncodeToString(n[:])
}

// MarshalText as 64 lowercase hexadecimal digits.
func (n Name) MarshalText() ([]byte, error) {
	return []byte(n.String()), nil
}

// UnmarshalText from 64 lowercase hexadecimal digits, as [ParseName] does.
func (n *Name) UnmarshalText(b []byte) error {
	parsed, err := ParseName(string(b))
	if err != nil {
		return err
	}
	*n = parsed
	return nil
}
