package stmttest

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Sequence of random statements of slice 1 that meet at a few tables and columns, each named in
// several spellings that SQLite takes for one name and in some that it takes for others, with
// values from a small set so that keys repeat, and with statements that are not supported and texts
// that SQLite refuses mixed in.
type Sequence struct {
	r       Rand
	g       *Generator
	tables  []string
	columns []string
}

// spellings of the name raw: itself, and other spellings in and without quotes, of which those that
// change only ASCII letters name the same name, and an upper case that changes other letters names
// another.
func spellings(r Rand, raw string) []string {
	if raw[0] == '"' {
		v := strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
		return []string{raw, Quote(swapCase(v)), Quote(strings.ToUpper(v)), Quote(randomCase(r, v))}
	}
	return []string{raw, strings.ToUpper(raw), randomCase(r, raw), Quote(raw), Quote(randomCase(r, raw))}
}

// swapCase of the ASCII letters of s.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// randomCase of the ASCII letters of s.
func randomCase(r Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if r.IntN(2) == 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// commonPools are the names that SQLite reads as a name of a table in every position of a table, and
// those that it reads as a name of a column in every position of a column.
var commonPools = sync.OnceValues(func() ([2][]string, error) {
	pools, err := NamePools()
	if err != nil {
		return [2][]string{}, err
	}
	return [2][]string{
		common(pools, CreateTable, InsertTable, SelectTable, DropTable),
		common(pools, CreateColumn, InsertColumn),
	}, nil
})

// common names of the pools at the positions.
func common(pools [][]string, positions ...Position) []string {
	var names []string
	for _, n := range pools[positions[0]] {
		in := true
		for _, p := range positions[1:] {
			in = in && slices.Contains(pools[p], n)
		}
		if in {
			names = append(names, n)
		}
	}
	return names
}

// NewSequence with the random numbers of r. It fails if [NamePools] fails.
func NewSequence(r Rand) (*Sequence, error) {
	common, err := commonPools()
	if err != nil {
		return nil, err
	}
	q := &Sequence{r: r}
	tableNames, columnNames := common[0], common[1]
	for range 1 + r.IntN(2) {
		q.tables = append(q.tables, spellings(r, tableNames[r.IntN(len(tableNames))])...)
	}
	for range 1 + r.IntN(2) {
		q.columns = append(q.columns, spellings(r, columnNames[r.IntN(len(columnNames))])...)
	}
	pools := make([][]string, len(PositionTexts))
	for _, p := range []Position{CreateTable, InsertTable, SelectTable, DropTable} {
		pools[p] = q.tables
	}
	pools[CreateColumn], pools[InsertColumn] = q.columns, q.columns
	q.g = NewGenerator(r, pools)
	q.g.Value = func() int64 {
		if r.IntN(4) == 0 {
			return Integer(r)
		}
		return int64(r.IntN(5)) - 1
	}
	return q, nil
}

// Others are texts that are not statements of slice 1, with %[1]v for a table, %[2]v for a column,
// and %[3]v for a value: statements that the pass does not support, texts that SQLite refuses with a
// syntax error, and texts without a statement.
var Others = []string{
	"create table %[1]v (%[2]v integer primary key) strict, without rowid",
	"create table %[1]v (%[2]v int primary key) strict",
	"create table if not exists %[1]v (%[2]v int primary key) strict, without rowid",
	`create table "" (%[2]v int primary key) strict, without rowid`,
	`create table %[1]v ("" int primary key) strict, without rowid`,
	"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
	"insert or replace into %[1]v (%[2]v) values (%[3]v)",
	"insert into %[1]v (%[2]v) values (+%[3]v)",
	"insert into %[1]v values (%[3]v)",
	`insert into "" (%[2]v) values (%[3]v)`,
	"select * from %[1]v where %[2]v = %[3]v",
	"select %[2]v from %[1]v",
	`select * from ""`,
	"drop table if exists %[1]v",
	"update %[1]v set %[2]v = %[3]v",
	"begin",
	"select * from %[1]v; select * from %[1]v",
	"select * frm %[1]v",
	"insert into %[1]v (%[2]v) values (",
	"create table %[1]v (%[2]v int primary key) strict without rowid",
	"drop table",
	"",
	" ; -- nothing",
}

// Next statement of the sequence, whether to run it as a query, and whether it is a statement of
// slice 1, which s4ql must support.
func (q *Sequence) Next() (src string, query, supported bool) {
	r := q.r
	if r.IntN(12) == 0 {
		form := Others[r.IntN(len(Others))]
		table, column, value := q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))], r.IntN(5)
		query := r.IntN(2) == 0
		if !strings.Contains(form, "%") {
			// Sprintf would add the arguments that a form without a verb does not use to its text.
			return form, query, false
		}
		return fmt.Sprintf(form, table, column, value), query, false
	}
	var k Kind
	switch n := r.IntN(20); {
	case n < 4:
		k = KindCreateTable
	case n < 14:
		k = KindInsert
	case n < 19:
		k = KindSelect
	default:
		k = KindDropTable
	}
	src, _ = q.g.StatementOf(k)
	// A query runs through Exec one time in eight, and every other statement through Query one time
	// in eight.
	return src, (k == KindSelect) != (r.IntN(8) == 0), true
}
