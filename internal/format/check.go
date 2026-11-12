package format

import (
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

// checkKnown that m and every message in it hold only fields and enum values that this version
// knows. A field or an enum value it does not know is from a newer version of s4ql, and the error
// wraps [ErrNewerVersion]. The path names m in the error.
func checkKnown(m protoreflect.Message, path string) error {
	if unknown := m.GetUnknown(); len(unknown) > 0 {
		num, _, _ := protowire.ConsumeTag(unknown)
		return fmt.Errorf("%w: %v holds field %v, which this version of s4ql does not know", ErrNewerVersion, path, num)
	}

	var err error
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		fieldPath := path + "." + string(fd.Name())
		switch {
		case fd.IsList():
			list := v.List()
			for i := 0; i < list.Len() && err == nil; i++ {
				err = checkKnownValue(fd, list.Get(i), fmt.Sprintf("%v[%v]", fieldPath, i))
			}
		default:
			err = checkKnownValue(fd, v, fieldPath)
		}
		return err == nil
	})
	return err
}

// checkKnownValue of the field fd, which is a single value: of a field that is not repeated, or one
// element of a repeated field.
func checkKnownValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, path string) error {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return checkKnown(v.Message(), path)
	case protoreflect.EnumKind:
		if fd.Enum().Values().ByNumber(v.Enum()) == nil {
			return fmt.Errorf("%w: %v is %v, which this version of s4ql does not know", ErrNewerVersion, path, v.Enum())
		}
	}
	return nil
}

// checkHeader of a commit object: its fields are known, its version is the one this package reads,
// its parent is a name, and its commit time is set and valid.
func checkHeader(h *s4qlpb.Header) error {
	if err := checkKnown(h.ProtoReflect(), "header"); err != nil {
		return err
	}
	if !h.HasVersion() {
		return fmt.Errorf("%w: header has no version", ErrMalformed)
	}
	if h.GetVersion() != commitVersion {
		return fmt.Errorf("%w: header has version %v, and this version of s4ql reads version %v", ErrNewerVersion, h.GetVersion(), commitVersion)
	}
	if !h.HasParent() {
		return fmt.Errorf("%w: header has no parent", ErrMalformed)
	}
	if _, err := NameFromBytes(h.GetParent()); err != nil {
		return fmt.Errorf("header parent: %w", err)
	}
	if !h.HasCommitTime() {
		return fmt.Errorf("%w: header has no commit time", ErrMalformed)
	}
	if err := h.GetCommitTime().CheckValid(); err != nil {
		return fmt.Errorf("%w: header commit time: %v", ErrMalformed, err)
	}
	return nil
}

// checkChange holds only what this version knows, and is one of the known kinds of change, each
// complete and consistent in itself. The path names the change in the error.
func checkChange(c *s4qlpb.Change, path string) error {
	if err := checkKnown(c.ProtoReflect(), path); err != nil {
		return err
	}

	switch c.WhichKind() {
	case s4qlpb.Change_CreateTable_case:
		return checkCreateTable(c.GetCreateTable(), path+".create_table")

	case s4qlpb.Change_DropTable_case:
		dt := c.GetDropTable()
		return checkIdentifier(dt.GetTable(), path+".drop_table.table")

	case s4qlpb.Change_UpsertRows_case:
		ur := c.GetUpsertRows()
		path += ".upsert_rows"
		if err := checkIdentifier(ur.GetTable(), path+".table"); err != nil {
			return err
		}
		if err := checkColumnPositions(ur.GetColumns(), path+".columns"); err != nil {
			return err
		}
		if len(ur.GetRows()) == 0 {
			return fmt.Errorf("%w: %v has no rows", ErrMalformed, path)
		}
		for i, row := range ur.GetRows() {
			if err := checkRow(len(ur.GetColumns()), row.GetValues(), fmt.Sprintf("%v.rows[%v]", path, i)); err != nil {
				return err
			}
		}
		return nil

	case s4qlpb.Change_DeleteRows_case:
		dr := c.GetDeleteRows()
		path += ".delete_rows"
		if err := checkIdentifier(dr.GetTable(), path+".table"); err != nil {
			return err
		}
		keys := dr.GetPrimaryKeys()
		if len(keys) == 0 {
			return fmt.Errorf("%w: %v has no primary keys", ErrMalformed, path)
		}
		for i, key := range keys {
			if err := checkPrimaryKey(len(keys[0].GetValues()), key.GetValues(), fmt.Sprintf("%v.primary_keys[%v]", path, i)); err != nil {
				return err
			}
		}
		return nil

	default:
		// checkKnown found no field it does not know, so the change holds nothing.
		return fmt.Errorf("%w: %v has no kind", ErrMalformed, path)
	}
}

// checkCreateTable has a name, at least one column, columns with names and types, and a primary key
// of distinct positions in its columns.
func checkCreateTable(ct *s4qlpb.CreateTable, path string) error {
	if err := checkIdentifier(ct.GetTable(), path+".table"); err != nil {
		return err
	}

	columns := ct.GetColumns()
	if len(columns) == 0 {
		return fmt.Errorf("%w: %v has no columns", ErrMalformed, path)
	}
	// SQLite compares the names of columns without regard to the case of ASCII letters.
	names := make(map[string]int, len(columns))
	for i, column := range columns {
		columnPath := fmt.Sprintf("%v.columns[%v]", path, i)
		if err := checkIdentifier(column.GetName(), columnPath+".name"); err != nil {
			return err
		}
		folded := asciiLower(column.GetName())
		if j, ok := names[folded]; ok {
			return fmt.Errorf("%w: %v has the name %q, as column %v does", ErrMalformed, columnPath, column.GetName(), j)
		}
		names[folded] = i
		if column.GetType() == s4qlpb.Type_TYPE_UNSPECIFIED {
			return fmt.Errorf("%w: %v has no type", ErrMalformed, columnPath)
		}
	}

	key := ct.GetPrimaryKey()
	if len(key) == 0 {
		return fmt.Errorf("%w: %v has no primary key", ErrMalformed, path)
	}
	seen := make(map[uint32]bool, len(key))
	for i, position := range key {
		if position >= uint32(len(columns)) {
			return fmt.Errorf("%w: %v.primary_key[%v] is column %v of %v", ErrMalformed, path, i, position, len(columns))
		}
		if seen[position] {
			return fmt.Errorf("%w: %v.primary_key[%v] lists column %v twice", ErrMalformed, path, i, position)
		}
		seen[position] = true
	}
	return nil
}

// checkColumnPositions of an [s4qlpb.UpsertRows]: at least one, and none twice.
func checkColumnPositions(columns []uint32, path string) error {
	if len(columns) == 0 {
		return fmt.Errorf("%w: %v is empty", ErrMalformed, path)
	}
	seen := make(map[uint32]bool, len(columns))
	for i, position := range columns {
		if seen[position] {
			return fmt.Errorf("%w: %v[%v] lists column %v twice", ErrMalformed, path, i, position)
		}
		seen[position] = true
	}
	return nil
}

// checkRow has a value for each of the columns of its batch, and each value is valid.
func checkRow(columns int, values []*s4qlpb.Value, path string) error {
	if len(values) != columns {
		return fmt.Errorf("%w: %v has %v values for %v columns", ErrMalformed, path, len(values), columns)
	}
	return checkValues(values, path)
}

// checkPrimaryKey has at least one value, as many as the first key of its batch, and each value is
// valid.
func checkPrimaryKey(want int, values []*s4qlpb.Value, path string) error {
	if len(values) == 0 {
		return fmt.Errorf("%w: %v has no values", ErrMalformed, path)
	}
	if len(values) != want {
		return fmt.Errorf("%w: %v has %v values, and the first key of its batch has %v", ErrMalformed, path, len(values), want)
	}
	return checkValues(values, path)
}

func checkValues(values []*s4qlpb.Value, path string) error {
	for i, v := range values {
		if err := checkValue(v, fmt.Sprintf("%v.values[%v]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

// checkValue is one of SQLite's values: it has a known kind, a real is not NaN, which SQLite cannot
// store, and text is valid UTF-8. The caller runs [checkKnown] on it first.
func checkValue(v *s4qlpb.Value, path string) error {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case, s4qlpb.Value_Blob_case, s4qlpb.Value_Null_case:
		return nil
	case s4qlpb.Value_Real_case:
		if math.IsNaN(v.GetReal()) {
			return fmt.Errorf("%w: %v is a real that is NaN", ErrMalformed, path)
		}
		return nil
	case s4qlpb.Value_Text_case:
		if !utf8.ValidString(v.GetText()) {
			return fmt.Errorf("%w: %v is text that is not valid UTF-8", ErrMalformed, path)
		}
		return nil
	default:
		// The caller has run checkKnown, which found no field it does not know, so the value holds
		// nothing.
		return fmt.Errorf("%w: %v has no kind", ErrMalformed, path)
	}
}

// checkIdentifier, the name of a table or a column: not empty, valid UTF-8, and without the
// character NUL, at which SQLite's C interface ends a name.
func checkIdentifier(name, path string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: %v is empty", ErrMalformed, path)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: %v is not valid UTF-8", ErrMalformed, path)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: %v %q holds the character NUL", ErrMalformed, path, name)
	}
	return nil
}

// checkAnnotation for writing: it holds only what this version knows, it has a known kind, and its
// statement has valid text and valid parameters. A reader is more lenient, and skips what it does
// not know.
func checkAnnotation(a *s4qlpb.Annotation, path string) error {
	if err := checkKnown(a.ProtoReflect(), path); err != nil {
		return err
	}
	switch a.WhichKind() {
	case s4qlpb.Annotation_Statement_case:
		s := a.GetStatement()
		if !utf8.ValidString(s.GetSql()) {
			return fmt.Errorf("%w: %v.statement.sql is not valid UTF-8", ErrMalformed, path)
		}
		for i, p := range s.GetParameters() {
			if err := checkValue(p, fmt.Sprintf("%v.statement.parameters[%v]", path, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %v has no kind", ErrMalformed, path)
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
