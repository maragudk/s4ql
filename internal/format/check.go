package format

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// commitVersion is the version of the commit object format that this package reads and writes.
const commitVersion = 0

// checkError is a refusal of a commit object: whether it is malformed or from a newer version, where
// it is, and what is wrong. The checks build the path to where it is only when they refuse, from the
// innermost message outward, so that a check that passes builds no strings.
type checkError struct {
	kind error
	// where it is, innermost first.
	where []string
	what  string
}

func (e *checkError) Error() string {
	var b strings.Builder
	b.WriteString(e.kind.Error())
	b.WriteString(": ")
	for i := len(e.where) - 1; i >= 0; i-- {
		b.WriteString(e.where[i])
		if i > 0 {
			b.WriteByte('.')
		}
	}
	if len(e.where) > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(e.what)
	return b.String()
}

func (e *checkError) Unwrap() error {
	return e.kind
}

// refuse with an error of the given kind, [ErrMalformed] or [ErrNewerVersion], that says what is
// wrong.
func refuse(kind error, format string, args ...any) error {
	return &checkError{kind: kind, what: fmt.Sprintf(format, args...)}
}

// in the message or field named where: add it to the front of the path of err.
func in(err error, where string) error {
	var ce *checkError
	if errors.As(err, &ce) {
		ce.where = append(ce.where, where)
		return ce
	}
	return fmt.Errorf("%v: %w", where, err)
}

// inElement i of the repeated field: add it to the front of the path of err.
func inElement(err error, field string, i int) error {
	return in(err, fmt.Sprintf("%v[%v]", field, i))
}

// checkKnown that m and every message in it hold only fields and enum values that this version
// knows. A field or an enum value it does not know is from a newer version of s4ql, and the error
// wraps [ErrNewerVersion].
func checkKnown(m protoreflect.Message) error {
	if unknown := m.GetUnknown(); len(unknown) > 0 {
		num, _, _ := protowire.ConsumeTag(unknown)
		return refuse(ErrNewerVersion, "holds field %v, which this version of s4ql does not know", num)
	}

	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.IsList() {
			list := v.List()
			for i := 0; i < list.Len(); i++ {
				if err = checkKnownValue(fd, list.Get(i)); err != nil {
					err = inElement(err, string(fd.Name()), i)
					return false
				}
			}
			return true
		}
		if err = checkKnownValue(fd, v); err != nil {
			err = in(err, string(fd.Name()))
			return false
		}
		return true
	})
	return err
}

// checkKnownValue of the field fd, which is a single value: of a field that is not repeated, or one
// element of a repeated field.
func checkKnownValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return checkKnown(v.Message())
	case protoreflect.EnumKind:
		if fd.Enum().Values().ByNumber(v.Enum()) == nil {
			return refuse(ErrNewerVersion, "is %v, which this version of s4ql does not know", v.Enum())
		}
	}
	return nil
}

// checkHeader of a commit object: its fields are known, its version is the one this package reads,
// its parent is a name, and its commit time is set and valid.
func checkHeader(h *s4qlpb.Header) error {
	if err := checkKnown(h.ProtoReflect()); err != nil {
		return in(err, "header")
	}
	if !h.HasVersion() {
		return in(refuse(ErrMalformed, "has no version"), "header")
	}
	if h.GetVersion() != commitVersion {
		return in(refuse(ErrNewerVersion, "has version %v, and this version of s4ql reads version %v", h.GetVersion(), commitVersion), "header")
	}
	if !h.HasParent() {
		return in(refuse(ErrMalformed, "has no parent"), "header")
	}
	if len(h.GetParent()) != len(Name{}) {
		return in(refuse(ErrMalformed, "is %v bytes, not 32", len(h.GetParent())), "header.parent")
	}
	if !h.HasCommitTime() {
		return in(refuse(ErrMalformed, "has no commit time"), "header")
	}
	if err := h.GetCommitTime().CheckValid(); err != nil {
		return in(refuse(ErrMalformed, "is not valid: %v", err), "header.commit_time")
	}
	return nil
}

// checkChange holds only what this version knows, and is one of the known kinds of change, each
// complete and consistent in itself.
func checkChange(c *s4qlpb.Change) error {
	if err := checkKnown(c.ProtoReflect()); err != nil {
		return err
	}

	switch c.WhichKind() {
	case s4qlpb.Change_CreateTable_case:
		if err := checkCreateTable(c.GetCreateTable()); err != nil {
			return in(err, "create_table")
		}
		return nil

	case s4qlpb.Change_DropTable_case:
		if err := checkIdentifier(c.GetDropTable().GetTable()); err != nil {
			return in(err, "drop_table.table")
		}
		return nil

	case s4qlpb.Change_UpsertRows_case:
		if err := checkUpsertRows(c.GetUpsertRows()); err != nil {
			return in(err, "upsert_rows")
		}
		return nil

	case s4qlpb.Change_DeleteRows_case:
		if err := checkDeleteRows(c.GetDeleteRows()); err != nil {
			return in(err, "delete_rows")
		}
		return nil

	default:
		// checkKnown found no field it does not know, so the change holds nothing.
		return refuse(ErrMalformed, "has no kind")
	}
}

// checkCreateTable has a name, at least one column, columns with names and types, and a primary key
// of distinct positions in its columns.
func checkCreateTable(ct *s4qlpb.CreateTable) error {
	if err := checkIdentifier(ct.GetTable()); err != nil {
		return in(err, "table")
	}

	columns := ct.GetColumns()
	if len(columns) == 0 {
		return refuse(ErrMalformed, "has no columns")
	}
	// SQLite compares the names of columns without regard to the case of ASCII letters.
	names := make(map[string]int, len(columns))
	for i, column := range columns {
		if err := checkIdentifier(column.GetName()); err != nil {
			return inElement(in(err, "name"), "columns", i)
		}
		folded := asciiLower(column.GetName())
		if j, ok := names[folded]; ok {
			return inElement(refuse(ErrMalformed, "has the name %q, as column %v does", column.GetName(), j), "columns", i)
		}
		names[folded] = i
		if column.GetType() == s4qlpb.Type_TYPE_UNSPECIFIED {
			return inElement(refuse(ErrMalformed, "has no type"), "columns", i)
		}
	}

	key := ct.GetPrimaryKey()
	if len(key) == 0 {
		return refuse(ErrMalformed, "has no primary key")
	}
	seen := make(map[uint32]bool, len(key))
	for i, position := range key {
		if position >= uint32(len(columns)) {
			return inElement(refuse(ErrMalformed, "is column %v of %v", position, len(columns)), "primary_key", i)
		}
		if seen[position] {
			return inElement(refuse(ErrMalformed, "lists column %v twice", position), "primary_key", i)
		}
		seen[position] = true
	}
	return nil
}

// checkUpsertRows has a table name, columns, and at least one row, and each row is valid for the
// columns.
func checkUpsertRows(ur *s4qlpb.UpsertRows) error {
	if err := checkIdentifier(ur.GetTable()); err != nil {
		return in(err, "table")
	}
	if err := checkColumnPositions(ur.GetColumns()); err != nil {
		return err
	}
	if len(ur.GetRows()) == 0 {
		return refuse(ErrMalformed, "has no rows")
	}
	for i, row := range ur.GetRows() {
		if err := checkRow(len(ur.GetColumns()), row.GetValues()); err != nil {
			return inElement(err, "rows", i)
		}
	}
	return nil
}

// checkDeleteRows has a table name and at least one primary key, and each key is valid and has as
// many values as the first.
func checkDeleteRows(dr *s4qlpb.DeleteRows) error {
	if err := checkIdentifier(dr.GetTable()); err != nil {
		return in(err, "table")
	}
	keys := dr.GetPrimaryKeys()
	if len(keys) == 0 {
		return refuse(ErrMalformed, "has no primary keys")
	}
	for i, key := range keys {
		if err := checkPrimaryKey(len(keys[0].GetValues()), key.GetValues()); err != nil {
			return inElement(err, "primary_keys", i)
		}
	}
	return nil
}

// checkColumnPositions of an [s4qlpb.UpsertRows]: at least one, and none twice.
func checkColumnPositions(columns []uint32) error {
	if len(columns) == 0 {
		return in(refuse(ErrMalformed, "is empty"), "columns")
	}
	seen := make(map[uint32]bool, len(columns))
	for i, position := range columns {
		if seen[position] {
			return inElement(refuse(ErrMalformed, "lists column %v twice", position), "columns", i)
		}
		seen[position] = true
	}
	return nil
}

// checkRow has a value for each of the columns of its batch, and each value is valid.
func checkRow(columns int, values []*s4qlpb.Value) error {
	if len(values) != columns {
		return refuse(ErrMalformed, "has %v values for %v columns", len(values), columns)
	}
	return checkValues(values)
}

// checkPrimaryKey has at least one value, as many as the first key of its batch, and each value is
// valid.
func checkPrimaryKey(want int, values []*s4qlpb.Value) error {
	if len(values) == 0 {
		return refuse(ErrMalformed, "has no values")
	}
	if len(values) != want {
		return refuse(ErrMalformed, "has %v values, and the first key of its batch has %v", len(values), want)
	}
	return checkValues(values)
}

func checkValues(values []*s4qlpb.Value) error {
	for i, v := range values {
		if err := checkValue(v); err != nil {
			return inElement(err, "values", i)
		}
	}
	return nil
}

// checkValue is one of SQLite's values: it has a known kind, a real is not NaN, which SQLite cannot
// store, and text is valid UTF-8. The caller runs [checkKnown] on it first.
func checkValue(v *s4qlpb.Value) error {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case, s4qlpb.Value_Blob_case, s4qlpb.Value_Null_case:
		return nil
	case s4qlpb.Value_Real_case:
		if math.IsNaN(v.GetReal()) {
			return refuse(ErrMalformed, "is a real that is NaN")
		}
		return nil
	case s4qlpb.Value_Text_case:
		if !utf8.ValidString(v.GetText()) {
			return refuse(ErrMalformed, "is text that is not valid UTF-8")
		}
		return nil
	default:
		// The caller has run checkKnown, which found no field it does not know, so the value holds
		// nothing.
		return refuse(ErrMalformed, "has no kind")
	}
}

// checkIdentifier, the name of a table or a column: not empty, valid UTF-8, and without the
// character NUL, at which SQLite's C interface ends a name.
func checkIdentifier(name string) error {
	switch {
	case name == "":
		return refuse(ErrMalformed, "is empty")
	case !utf8.ValidString(name):
		return refuse(ErrMalformed, "is not valid UTF-8")
	case strings.ContainsRune(name, 0):
		return refuse(ErrMalformed, "%q holds the character NUL", name)
	}
	return nil
}

// checkAnnotation for writing: it holds only what this version knows, it has a known kind, and its
// statement has valid text and valid parameters. A reader is more lenient, and skips what it does
// not know.
func checkAnnotation(a *s4qlpb.Annotation) error {
	if err := checkKnown(a.ProtoReflect()); err != nil {
		return err
	}
	switch a.WhichKind() {
	case s4qlpb.Annotation_Statement_case:
		s := a.GetStatement()
		if !utf8.ValidString(s.GetSql()) {
			return in(refuse(ErrMalformed, "is not valid UTF-8"), "statement.sql")
		}
		for i, p := range s.GetParameters() {
			if err := checkValue(p); err != nil {
				return in(inElement(err, "parameters", i), "statement")
			}
		}
		return nil
	default:
		return refuse(ErrMalformed, "has no kind")
	}
}

// asciiLower changes the ASCII letters in s to lowercase, and nothing else, as SQLite does when it
// compares identifiers.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}
