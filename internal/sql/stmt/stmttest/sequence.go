package stmttest

import (
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// Sequence of random statements of slices 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, and 12 that meet at a few
// tables and columns, each named in several spellings that SQLite takes for one name and in some
// that it takes for others, with values from a small set so that keys repeat, and with statements
// that are not supported and texts that SQLite refuses mixed in.
//
// A sequence is a function of its seed alone. The seed gives it its [Profile], as [ProfileOf]
// says, which weighs the kinds of statement and turns each area of the generator on or off, and
// every random number that it draws comes from the stream of an area, seeded from the seed, the
// name of the area, and the number of the statement or the name of the table that the number is
// for: see the areas in profile.go. What follows describes a sequence of [Mixed], in which every
// area is on; the fields of [Profile] say what another profile changes. In a profile with
// [Profile.Aim], most statements name a table that the sequence created and did not drop, as
// [Sequence.aim] says, and each fault below is a quarter as likely in the statements of an area that
// is not among [Profile.Faults], as [Sequence.fault] says.
//
// Each name of a table has a shape of the sequence's own: from one to [sequenceColumns] columns,
// each with a name and one of the five types, of which two often have the same type, the primary
// key at any position, of the other columns none, all, or some that take NULL, and of all its
// columns none, all, or some with a DEFAULT, as [Sequence.defaultOf] draws it. Nine create tables of
// the name in ten declare that shape, its names in any of their spellings, and one of these in ten
// gives one column any type, one in ten turns whether one column takes NULL, and one in ten gives one
// column another default or none; the tenth has columns at random, whose names can repeat, which
// SQLite refuses, each with a default one time in four. An insert into a table of the name names the
// columns of the shape in a list in any order, or one time in three without a list, in the order of
// the shape, with a value of the type of each column, or NULL now and then for a column that takes
// NULL, which a list also leaves out now and then, as it leaves out a column with a default one time
// in two, and the key with a default one time in four. Now and then it leaves any
// column out of its list, names a column that the shape may not have, has a value more or less, has
// a value of any type, which is mostly another than its column, for one column or for two, or has
// NULL for any column, also the key; so some inserts have two faults. A select names * or columns
// of the shape in any order, also one twice, and now and then a name that the shape may not have,
// and one time in two has a WHERE, as [Sequence.where] draws it, whose values are mostly those that
// the inserts gave its columns, their neighbours, and the edges of each type. The small sets of
// values hold keys that SQLite takes for one, such as 0.0 and -0.0, so that every type meets a key
// that exists. An update sets some columns of the shape, as [Sequence.update] draws it, and an
// update three times in four and a delete four times in five have a WHERE, as [Sequence.changeConditions]
// draws one: one time in two, if the sequence keeps a value of the key, the key equal to that value,
// or, for every third of those values, bounded on one side by it, and otherwise as a select has one.
// A create index names an index from a few names in several spellings, or now and then a name of a
// table, on a table of the sequence, and the columns of its shape, as [Sequence.createIndex] draws
// them; a drop index names one of those names, or now and then a name of a table; and a create table
// and a drop table name one of the names of an index now and then. One WHERE of a select in two of a
// table of which the sequence keeps an index, as [Sequence.indexed] says, and one in two of the WHERE
// of an update or a delete that are drawn as a select has one, pins leading columns of the index, or
// one time in eight only bounds its first column, as [Sequence.indexConditions] draws them. One
// create table, create index, drop table, and drop index in three says IF NOT EXISTS or IF EXISTS, as
// [Generator.Exists] writes it. One comparison in three of a WHERE has its value on the left and its
// column on the right, as [Generator.ValueFirst] writes it, with the conditions as the hooks give
// them.
//
// One value in [Profile.Parameters] is a plain ? with the value as its argument, nil for NULL, and
// some calls, as [Sequence.Next] says, have arguments around those that s4ql supports: none for a
// ?, one for a statement without one, two for a ?, one that no column holds, one with a name, or a
// value of another Go type that s4ql supports.
type Sequence struct {
	seed    uint64
	profile *Profile
	// step is the number of the statement that the sequence writes, from 1, and 0 before the first.
	step uint64
	// The streams of the areas that start anew at each statement, as [stepStream] says. The stream
	// of an area that is off in the profile panics at a draw.
	rPlan, rTables, rInserts, rSelects, rChanges, rIndexes, rDefaults, rArguments, rOthers Rand
	g                                                                                      *Generator
	tables                                                                                 []string
	// columns are the spellings of the names of columns, folds the folds of their names, in the order
	// that the spellings first have them, and spelled the spellings of each fold.
	columns []string
	folds   []string
	spelled map[string][]string
	// shapes of the tables by their names, folded as SQLite compares names: the columns of each in
	// the order of the table, with the fold of each name as its Name.Value.
	shapes map[string][]stmt.Column
	// stored are values that the inserts of the sequence gave, by the folds of the names of their
	// table and their column, the last [storedValues] of each, whether SQLite took the insert or not,
	// for the values of the conditions of a WHERE.
	stored map[[2]string][]stmt.Value
	// indexes are the spellings of the names of indexes.
	indexes []string
	// indexed holds the indexes that the sequence created and did not drop since, as far as its own
	// statements tell, by the fold of the name of their table, the last [storedIndexes] of each, with
	// the fold of the name of each column as its Name.Value, for the conditions of a WHERE that an
	// index can serve. A create index is kept if its table is live, its name is not that of a live
	// index or table, and each of its columns is one of the shape; SQLite may refuse it still, for a
	// table that it refused.
	indexed map[string][]sequenceIndex
	// pools of the names of the sequence, for each [Position], which the generator gets anew for each
	// statement, as [Sequence.aim] says.
	pools [][]string
	// live are the folds of the names of the tables for which the sequence wrote a create table, and
	// since then no drop table, whether SQLite took the statement or not.
	live map[string]bool
}

// Seed of the sequence.
func (q *Sequence) Seed() uint64 {
	return q.seed
}

// Profile of the sequence, which is that of its seed.
func (q *Sequence) Profile() *Profile {
	return q.profile
}

// Stream of random numbers of the name for the test that runs the sequence, such as the stream of
// its transactions or of its restarts: the same numbers for the same seed and name, whatever the
// sequence and the other streams draw, so that what a test draws for itself moves no statement. Each
// call gives the stream from its first number, so a test takes a stream once and keeps it.
func (q *Sequence) Stream(name string) Rand {
	return newStream(q.seed, "test", name)
}

// stream of the area that starts anew at each statement, or one that panics at a draw if the
// profile has the area off.
func (q *Sequence) stream(area string) Rand {
	if !q.profile.on(area) {
		return offStream{area: area, profile: q.profile.Name}
	}
	return newStepStream(q.seed, area, &q.step)
}

// entity is the stream of the area for the thing of the name what, such as the shape of a table,
// which is the same whichever statement asks for it first, or one that panics at a draw if the
// profile has the area off.
func (q *Sequence) entity(area, what string) Rand {
	if !q.profile.on(area) {
		return offStream{area: area, profile: q.profile.Name}
	}
	return newStream(q.seed, area, what)
}

// IndexNames of the sequence: the values of the names of its indexes, without their quotes, each
// once for every spelling that names it, as a path of an index names it.
func (q *Sequence) IndexNames() []string {
	var names []string
	for _, raw := range q.indexes {
		names = append(names, nameOf(raw, 0).Value)
	}
	return names
}

// sequenceIndex is an index that a [Sequence] keeps for its conditions: the fold of its name, and its
// columns.
type sequenceIndex struct {
	name    string
	columns []stmt.IndexColumn
}

// storedIndexes is the most create indexes of a table that a [Sequence] keeps for its conditions.
const storedIndexes = 3

// indexWhere reports whether the WHERE of a statement of table is one that an index of the
// sequence could serve, as [Sequence.indexConditions] draws it: for a table of which the sequence
// keeps an index, as [Sequence.indexed] says, one time in two, or three times in four with
// [Profile.Indexes] of 2.
func (q *Sequence) indexWhere(table stmt.Name) bool {
	if len(q.indexed[table.Fold()]) == 0 {
		return false
	}
	if q.profile.Indexes > 1 {
		return q.rIndexes.IntN(4) != 0
	}
	return q.rIndexes.IntN(2) == 0
}

// storedValues is the most values of a column that a [Sequence] keeps for its conditions.
const storedValues = 12

// sequenceColumns is the most columns of a table of a [Sequence].
const sequenceColumns = 3

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

// commonPools are the names that SQLite reads as a name of a table in every position of a table and
// of a create index, those that it reads as a name of a column in every position of a column, also
// of an index, and those that it reads as a name of an index in every position of an index and of a
// table.
var commonPools = sync.OnceValues(func() ([3][]string, error) {
	pools, err := NamePools()
	if err != nil {
		return [3][]string{}, err
	}
	return [3][]string{
		common(pools, CreateTable, InsertTable, SelectTable, DropTable, UpdateTable, DeleteTable, IndexTable, IndexName, DropIndex),
		common(pools, CreateColumn, InsertColumn, SelectColumn, UpdateColumn, IndexColumn),
		common(pools, IndexName, DropIndex, CreateTable, InsertTable, SelectTable, DropTable, UpdateTable, DeleteTable, IndexTable),
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

// NewSequence of the seed, with the profile of the seed, as [ProfileOf] gives it. It fails if
// [NamePools] fails.
func NewSequence(seed uint64) (*Sequence, error) {
	return newSequence(seed, ProfileOf(seed))
}

// newSequence of the seed with the profile p.
func newSequence(seed uint64, p *Profile) (*Sequence, error) {
	common, err := commonPools()
	if err != nil {
		return nil, err
	}
	q := &Sequence{seed: seed, profile: p, spelled: map[string][]string{}, shapes: map[string][]stmt.Column{}, stored: map[[2]string][]stmt.Value{}, indexed: map[string][]sequenceIndex{}}
	q.rPlan, q.rTables, q.rInserts, q.rSelects, q.rChanges = q.stream(areaPlan), q.stream(areaTables), q.stream(areaInserts), q.stream(areaSelects), q.stream(areaChanges)
	q.rIndexes, q.rDefaults, q.rArguments, q.rOthers = q.stream(areaIndexes), q.stream(areaDefaults), q.stream(areaArguments), q.stream(areaOthers)
	r := q.entity(areaNames, "")
	tableNames, columnNames := common[0], common[1]
	for range 1 + r.IntN(2) {
		q.tables = append(q.tables, spellings(r, tableNames[r.IntN(len(tableNames))])...)
	}
	// So many names of columns that a table of the most columns lacks one of them.
	for len(q.folds) <= sequenceColumns {
		for _, raw := range spellings(r, columnNames[r.IntN(len(columnNames))]) {
			q.columns = append(q.columns, raw)
			fold := nameOf(raw, 0).Fold()
			if q.spelled[fold] == nil {
				q.folds = append(q.folds, fold)
			}
			q.spelled[fold] = append(q.spelled[fold], raw)
		}
	}
	pools := make([][]string, len(PositionTexts))
	for _, position := range []Position{CreateTable, InsertTable, SelectTable, DropTable, UpdateTable, DeleteTable, IndexTable, IndexName, DropIndex} {
		pools[position] = q.tables
	}
	pools[CreateColumn], pools[InsertColumn], pools[SelectColumn], pools[UpdateColumn], pools[IndexColumn] = q.columns, q.columns, q.columns, q.columns, q.columns
	if p.Indexes > 0 {
		// The names of indexes, of which one is also among the names of a create table and a drop table,
		// and one name of a table among those of a create index and a drop index, so that a table and an
		// index meet at a name.
		r := q.entity(areaIndexes, "")
		indexNames := common[2]
		// One or two names, and two or three with [Profile.Indexes] of 2, so that a table has several
		// indexes.
		for range p.Indexes + r.IntN(2) {
			q.indexes = append(q.indexes, spellings(r, indexNames[r.IntN(len(indexNames))])...)
		}
		pools[CreateTable] = append(slices.Clone(q.tables), q.indexes[r.IntN(len(q.indexes))])
		pools[DropTable] = append(slices.Clone(q.tables), q.indexes[r.IntN(len(q.indexes))])
		pools[IndexName] = append(slices.Clone(q.indexes), q.tables[r.IntN(len(q.tables))])
		pools[DropIndex] = append(slices.Clone(q.indexes), q.tables[r.IntN(len(q.tables))])
	}
	q.pools, q.live = pools, map[string]bool{}
	q.g = NewGenerator(q.stream(areaText), slices.Clone(pools))
	q.g.plan, q.g.clause, q.g.params, q.g.turned = q.rPlan, q.stream(areaExists), q.rArguments, q.stream(areaValueFirst)
	q.g.Columns, q.g.Create, q.g.Insert, q.g.Select, q.g.Where = sequenceColumns, q.create, q.insert, q.selectList, q.where
	q.g.Update, q.g.Delete, q.g.CreateIndex = q.update, q.delete, q.createIndex
	q.g.Parameters, q.g.Exists, q.g.ValueFirst = p.Parameters, p.Exists, p.ValueFirst
	return q, nil
}

// shapeOf the tables of the name of table: the sequence's own, chosen at random the first time.
func (q *Sequence) shapeOf(table stmt.Name) []stmt.Column {
	name := table.Fold()
	shape, ok := q.shapes[name]
	if ok {
		return shape
	}
	r := q.entity(areaTables, name)
	folds := slices.Clone(q.folds)
	n := max(1+r.IntN(sequenceColumns), q.profile.Columns)
	key := r.IntN(n)
	for i := range n {
		j := i + r.IntN(len(folds)-i)
		folds[i], folds[j] = folds[j], folds[i]
		typ := Types[r.IntN(len(Types))]
		if i > 0 && r.IntN(2) == 0 {
			// Two columns of one type, so that a value in the place of the other has the type of its
			// column.
			typ = shape[r.IntN(i)].Type
		}
		shape = append(shape, stmt.Column{Name: stmt.Name{Value: folds[i]}, Type: typ, PrimaryKey: i == key})
	}
	// Of the columns outside the key, none takes NULL, every one does, or each one time in two.
	nulls := r.IntN(3)
	for i := range shape {
		shape[i].NotNull = !shape[i].PrimaryKey && (nulls == 0 || nulls == 2 && r.IntN(2) == 0)
	}
	if q.profile.Defaults > 0 {
		// Of all the columns, none has a default, every one has, or each one time in two, and with
		// [Profile.Defaults] of 2 every one has, or each one time in two.
		d := q.entity(areaDefaults, name)
		defaults := 1 + d.IntN(2)
		if q.profile.Defaults == 1 {
			defaults = d.IntN(3)
		}
		for i := range shape {
			if defaults == 1 || defaults == 2 && d.IntN(2) == 0 {
				shape[i].Default.Value = q.defaultOf(d, shape[i].Type)
			}
		}
	}
	q.shapes[name] = shape
	return shape
}

// defaultOf a column of the type t, for its DEFAULT: NULL one time in six, also for the key and a
// column that does not take NULL, so that an insert that leaves the column out meets SQLite's NOT
// NULL; for text and a blob one time in three a long value, of about [LengthLimit]/3 bytes of text
// or [LengthLimit]/5 bytes of a blob, whose literal is twice as long for a blob, so that the record of
// a row with it and another long value is too big; and otherwise a value as an insert gives the
// column one, mostly from the small set, so that a default of the key meets a key that exists, but
// never longer than [LengthLimit]/2 bytes. A shape with two or three long defaults can be too big for
// the lowered limit, and its create table then gets `string or blob too big` in every create of the
// sequence, which is one of the outcomes that the tests count.
func (q *Sequence) defaultOf(r Rand, t stmt.Type) stmt.Value {
	if r.IntN(6) == 0 {
		return stmt.Null{}
	}
	switch {
	case t == stmt.TypeText && r.IntN(3) == 0:
		return stmt.Text(strings.Repeat("d", LengthLimit/3-10+r.IntN(20)))
	case t == stmt.TypeBlob && r.IntN(3) == 0:
		return stmt.Blob(strings.Repeat("d", LengthLimit/5+r.IntN(20)))
	}
	switch v := q.valueOf(r, t).(type) {
	case stmt.Text:
		if len(v) > LengthLimit/2 {
			return stmt.Text(strings.ToValidUTF8(string(v[:LengthLimit/4]), ""))
		}
		return v
	case stmt.Blob:
		if len(v) > LengthLimit/5 {
			return v[:LengthLimit/10]
		}
		return v
	default:
		return v
	}
}

// spell the name of a column of the fold in one of its spellings.
func (q *Sequence) spell(r Rand, fold string) string {
	raws := q.spelled[fold]
	return raws[r.IntN(len(raws))]
}

// anyType at random.
func anyType(r Rand) stmt.Type {
	return Types[r.IntN(len(Types))]
}

// leavesOutLong reports whether a list of the positions order of the columns of the shape leaves out
// a column whose default is text or a blob of [LengthLimit]/5 bytes or more, as [Sequence.defaultOf]
// gives a long one.
func leavesOutLong(shape []stmt.Column, order []int) bool {
	for position, c := range shape {
		if slices.Contains(order, position) {
			continue
		}
		switch v := c.Default.Value.(type) {
		case stmt.Text:
			if len(v) >= LengthLimit/5 {
				return true
			}
		case stmt.Blob:
			if len(v) >= LengthLimit/5 {
				return true
			}
		}
	}
	return false
}

// create gives the columns of a create table of table, as [Generator.Create] does: those of its
// shape, in any of their spellings, and one time in ten with one column of any type, whose default,
// if it has one, is then one of that type, one time in ten with one column that takes NULL where the
// shape's does not or the other way round, which is no change for the key, and one time in ten with
// one column whose default is another or none, so that a table that is created again can have other
// defaults; or one time in ten columns at random, whose names can repeat, of which each outside the
// key takes NULL one time in two, and each has a default one time in four.
func (q *Sequence) create(table stmt.Name) []stmt.Column {
	r, d := q.rTables, q.rDefaults
	shape := q.shapeOf(table)
	// A create index names a table that a create table named more often than one that none named,
	// since a create index of a table that is not there only fails.
	if pool := &q.pools[IndexTable]; len(*pool) < 4*len(q.tables) {
		*pool = append(slices.Clip(*pool), table.Raw, table.Raw)
	}
	if q.fault(r, areaTables, 10) {
		n := 1 + r.IntN(sequenceColumns)
		key := r.IntN(n)
		columns := make([]stmt.Column, n)
		for i := range columns {
			columns[i] = stmt.Column{Name: stmt.Name{Raw: q.columns[r.IntN(len(q.columns))]}, Type: anyType(r), PrimaryKey: i == key, NotNull: r.IntN(2) == 0}
			if q.profile.Defaults > 0 && d.IntN(4) == 0 {
				columns[i].Default.Value = q.defaultOf(d, columns[i].Type)
			}
		}
		return columns
	}
	retyped, renulled, redefaulted := -1, -1, -1
	if r.IntN(10) == 0 {
		retyped = r.IntN(len(shape))
	}
	if r.IntN(10) == 0 {
		renulled = r.IntN(len(shape))
	}
	if q.profile.Defaults > 0 && d.IntN(10) == 0 {
		redefaulted = d.IntN(len(shape))
	}
	var columns []stmt.Column
	for i, c := range shape {
		c.Name = stmt.Name{Raw: q.spell(r, c.Name.Value)}
		if i == retyped {
			c.Type = anyType(r)
			if _, null := c.Default.Value.(stmt.Null); c.Default.Value != nil && !null {
				// A default of another type than its column is not supported.
				c.Default.Value = q.defaultOf(d, c.Type)
			}
		}
		if i == renulled {
			c.NotNull = !c.NotNull
		}
		if i == redefaulted {
			c.Default.Value = nil
			if d.IntN(3) != 0 {
				c.Default.Value = q.defaultOf(d, c.Type)
			}
		}
		columns = append(columns, c)
	}
	return columns
}

// insert gives the list of columns and the values of an insert into table, as [Generator.Insert]
// does: the columns of its shape, in a list in any order or without a list, with a value of the type
// of each, or NULL one time in four for a column that takes NULL, which a list also leaves out one
// time in three. One list in eight leaves a column out, one in twelve has a name in place of one of
// its own that is no other of the list, one insert in ten has a value of any type, and one in three
// of these a second, one in twelve has NULL for any column, also the key, and one in eight has a
// value more or a value less, each one time in two. These are the faults of the inserts, with the
// odds of a profile that has the inserts among [Profile.Faults]; in another profile each is a quarter
// as likely, as [Sequence.fault] says. A list that leaves out a column with a long default has, one
// time in two, a value of 85 hundredths of [LengthLimit] bytes for its first column of text or a
// blob. With [Profile.Mistyped], one insert in Mistyped also gives an integer of a column of INT or
// INTEGER as a real.
func (q *Sequence) insert(table stmt.Name) ([]string, []stmt.Value) {
	r, d := q.rInserts, q.rDefaults
	shape := q.shapeOf(table)
	order := make([]int, len(shape))
	for i := range order {
		order[i] = i
	}
	withList := r.IntN(3) != 0
	if withList {
		if r.IntN(3) != 0 {
			for i := len(order) - 1; i > 0; i-- {
				j := r.IntN(i + 1)
				order[i], order[j] = order[j], order[i]
			}
		}
		// A column that takes NULL is left out one time in three, and gets NULL or its default; a
		// column with a default is left out one time in two, and the key with a default one time in
		// four, and gets its default.
		order = slices.DeleteFunc(order, func(position int) bool {
			c := shape[position]
			switch {
			case !c.PrimaryKey && !c.NotNull && r.IntN(3) == 0:
				return true
			case c.Default.Value == nil:
				return false
			case c.PrimaryKey:
				return d.IntN(4) == 0
			}
			return d.IntN(2) == 0
		})
		if len(order) == 0 {
			// A list names a column at least: the key.
			order = []int{slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })}
		}
		if len(order) > 1 && q.fault(r, areaInserts, 8) {
			at := r.IntN(len(order))
			order = slices.Delete(order, at, at+1)
		}
	}
	var list []string
	var values []stmt.Value
	for _, position := range order {
		c := shape[position]
		if withList {
			list = append(list, q.spell(r, c.Name.Value))
		}
		if !c.PrimaryKey && !c.NotNull && r.IntN(4) == 0 {
			values = append(values, stmt.Null{})
			continue
		}
		values = append(values, q.valueOf(r, c.Type))
	}
	if withList && leavesOutLong(shape, order) && d.IntN(2) == 0 {
		// Beside a long default that the list leaves out, one time in two, a value of 85 hundredths of
		// the length limit for the first column of text or a blob of the list, which fits a row alone,
		// so that the default makes the record of the row too big.
		for i, position := range order {
			switch shape[position].Type {
			case stmt.TypeText:
				values[i] = stmt.Text(strings.Repeat("v", 85*LengthLimit/100))
			case stmt.TypeBlob:
				values[i] = stmt.Blob(strings.Repeat("v", 85*LengthLimit/100))
			default:
				continue
			}
			break
		}
	}
	if withList && q.fault(r, areaInserts, 12) {
		seen := map[string]bool{}
		for _, raw := range list {
			seen[nameOf(raw, 0).Fold()] = true
		}
		at := r.IntN(len(list))
		delete(seen, nameOf(list[at], 0).Fold())
		for range 100 {
			if raw := q.columns[r.IntN(len(q.columns))]; !seen[nameOf(raw, 0).Fold()] {
				list[at] = raw
				break
			}
		}
	}
	if q.fault(r, areaInserts, 10) {
		values[r.IntN(len(values))] = q.valueOf(r, anyType(r))
		if r.IntN(3) == 0 {
			values[r.IntN(len(values))] = q.valueOf(r, anyType(r))
		}
	}
	if q.fault(r, areaInserts, 12) {
		// NULL for any column: the key, one that does not take NULL, or one that does.
		values[r.IntN(len(values))] = stmt.Null{}
	}
	if q.fault(r, areaInserts, 8) {
		if r.IntN(2) == 0 {
			values = append(values, q.valueOf(r, anyType(r)))
		} else if len(values) > 1 {
			values = values[:len(values)-1]
		}
	}
	q.store(table, shape, list, values)
	if q.profile.Mistyped > 0 && r.IntN(q.profile.Mistyped) == 0 {
		// An integer of a column of INT or INTEGER as the real of its value, which the values of the
		// conditions keep as the integer. The column of a value is the one that the list names in its
		// place, which is another than the one of order where the list names another column.
		var integers []int
		for i, position := range order[:min(len(order), len(values))] {
			c := &shape[position]
			if withList {
				c = columnOfShape(shape, nameOf(list[i], 0).Fold())
			}
			if _, ok := values[i].(stmt.Integer); ok && c != nil && kindOf(c.Type) == stmt.TypeInt {
				integers = append(integers, i)
			}
		}
		if len(integers) > 0 {
			values = slices.Clone(values)
			at := integers[r.IntN(len(integers))]
			values[at] = stmt.Real(float64(values[at].(stmt.Integer)))
		}
	}
	return list, values
}

// store the values of an insert into table, whose shape is shape, with the list of columns list, or
// nil for the order of the shape, among the values of its columns that a WHERE draws from: each that
// is not NULL, for the column of its place, without a random number, so that the inserts draw as
// before.
func (q *Sequence) store(table stmt.Name, shape []stmt.Column, list []string, values []stmt.Value) {
	for i, v := range values {
		var column string
		switch {
		case list != nil && i < len(list):
			column = nameOf(list[i], 0).Fold()
		case list == nil && i < len(shape):
			column = shape[i].Name.Value
		default:
			continue
		}
		if _, ok := v.(stmt.Null); ok {
			continue
		}
		key := [2]string{table.Fold(), column}
		vs := q.stored[key]
		if len(vs) == storedValues {
			vs = vs[1:]
		}
		q.stored[key] = append(slices.Clip(vs), v)
	}
}

// selectList of a select of table, as [Generator.Select] gives it: * one time in two, and otherwise
// from one to [sequenceColumns]+1 names of the columns of its shape, of which one in four after the
// first is an earlier one, and one in twelve any name of a column of the sequence.
func (q *Sequence) selectList(table stmt.Name) []string {
	r := q.rSelects
	if r.IntN(2) == 0 {
		return nil
	}
	shape := q.shapeOf(table)
	var names []string
	for i := range 1 + r.IntN(sequenceColumns+1) {
		switch {
		case i > 0 && r.IntN(4) == 0:
			names = append(names, names[r.IntN(i)])
		case q.fault(r, areaSelects, 12):
			names = append(names, q.columns[r.IntN(len(q.columns))])
		default:
			names = append(names, q.spell(r, shape[r.IntN(len(shape))].Name.Value))
		}
	}
	return names
}

// comparisons are the operators of a condition that compare a column with a value.
var comparisons = []stmt.Operator{stmt.OpEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual}

// bounds are the operators of a condition that bound a column on one side.
var bounds = []stmt.Operator{stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual}

// inverses of the operators: the condition with the inverse of a condition's operator, on its column
// and with its value, is false wherever the condition is true.
var inverses = map[stmt.Operator]stmt.Operator{
	stmt.OpEqual: stmt.OpNotEqual, stmt.OpNotEqual: stmt.OpEqual, stmt.OpLess: stmt.OpGreaterOrEqual, stmt.OpGreaterOrEqual: stmt.OpLess,
	stmt.OpLessOrEqual: stmt.OpGreater, stmt.OpGreater: stmt.OpLessOrEqual, stmt.OpIsNull: stmt.OpIsNotNull, stmt.OpIsNotNull: stmt.OpIsNull,
}

// where gives the conditions of the WHERE of a select of table, as [Generator.Where] gives them:
// none for all but [Profile.Where] selects in four. Otherwise, where [Sequence.indexWhere] says so,
// those that [Sequence.indexConditions] draws for an index of the table. Otherwise, one time in five,
// a range of the key, a lower and an upper bound, each open or closed, in either order, of two values
// as [Sequence.conditionValue] draws them, which change places three times in four where they compare
// and the value of the lower bound is the greater, and from none to two more conditions; or else one
// or two conditions, or one time in eight from four to seven. The column of each such condition is
// that of an earlier condition one time in four, one that the shape does not have one time in ten,
// which can be in double quotes, and otherwise the key one time in three and any column of the shape,
// the key too, else. Its operator is IS NULL or IS NOT NULL one time in twelve each, and otherwise a
// comparison, with a value as [Sequence.conditionValue] draws it. One WHERE in eight then has one
// more condition, the inverse of one of its conditions, which contradicts it, and for a bound of the
// key leaves the range of the two without a key. The column that the shape does not have and the
// contradiction are faults, each a quarter as likely in a profile without the selects among
// [Profile.Faults], as [Sequence.fault] says.
func (q *Sequence) where(table stmt.Name) []stmt.Condition {
	if q.rSelects.IntN(4) >= q.profile.Where {
		return nil
	}
	if q.indexWhere(table) {
		return q.indexConditions(table)
	}
	return q.conditions(q.rSelects, areaSelects, table)
}

// conditions of a WHERE of a statement of table, one or more, as [Sequence.where] describes them for
// a WHERE that is not drawn for an index, with the random numbers of r, and with the faults of the
// area, as [Sequence.fault] draws them: a column that the shape does not have, a contradiction, and a
// value that is NULL or of another type than its column.
func (q *Sequence) conditions(r Rand, area string, table stmt.Name) []stmt.Condition {
	shape := q.shapeOf(table)
	key := &shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	var cs []stmt.Condition
	n := 1 + r.IntN(2)
	switch {
	case r.IntN(5) == 0:
		lower := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: []stmt.Operator{stmt.OpGreater, stmt.OpGreaterOrEqual}[r.IntN(2)]}
		upper := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: []stmt.Operator{stmt.OpLess, stmt.OpLessOrEqual}[r.IntN(2)]}
		lower.Value.Value, upper.Value.Value = q.conditionValue(r, area, table, key.Name.Value, key.Type), q.conditionValue(r, area, table, key.Name.Value, key.Type)
		a, b := termValue(lower.Value, nil), termValue(upper.Value, nil)
		if a != nil && b != nil && reflect.TypeOf(a) == reflect.TypeOf(b) && compareValues(a, b) > 0 && r.IntN(4) != 0 {
			lower.Value, upper.Value = upper.Value, lower.Value
		}
		cs = []stmt.Condition{lower, upper}
		if r.IntN(2) == 0 {
			cs[0], cs[1] = cs[1], cs[0]
		}
		n = r.IntN(3)
	case r.IntN(8) == 0:
		n = 4 + r.IntN(4)
	}
	for range n {
		var c stmt.Condition
		// column of the condition in the shape, or nil for one that the shape does not have.
		var column *stmt.Column
		switch {
		case len(cs) > 0 && r.IntN(4) == 0:
			c.Column.Raw = cs[r.IntN(len(cs))].Column.Raw
			if column = columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold()); column != nil {
				c.Column.Raw = q.spell(r, column.Name.Value)
			}
		case q.fault(r, area, 10):
			c.Column.Raw = q.missing(r, shape)
		case r.IntN(3) == 0:
			column = key
			c.Column.Raw = q.spell(r, column.Name.Value)
		default:
			column = &shape[r.IntN(len(shape))]
			c.Column.Raw = q.spell(r, column.Name.Value)
		}
		switch r.IntN(12) {
		case 0:
			c.Operator = stmt.OpIsNull
		case 1:
			c.Operator = stmt.OpIsNotNull
		default:
			c.Operator = comparisons[r.IntN(len(comparisons))]
			t, fold := anyType(r), ""
			if column != nil {
				t, fold = column.Type, column.Name.Value
			}
			c.Value.Value = q.conditionValue(r, area, table, fold, t)
		}
		cs = append(cs, c)
	}
	if len(cs) > 0 && q.fault(r, area, 8) {
		c := cs[r.IntN(len(cs))]
		c.Operator = inverses[c.Operator]
		cs = append(cs, c)
	}
	return cs
}

// createIndex gives the columns of a create index of the name index on table, as
// [Generator.CreateIndex] gives them: from one to all of the columns of the shape of table, each once,
// in any order, so that the key is among them now and then, or with [Profile.Indexes] of 2, three
// times in four, every column but the key of a shape of two columns or more, in any order; each
// descending one time in three. A name that the shape does not have, which can be in double quotes,
// is added at any place one time in twelve, and one time in forty, so that a few create indexes have
// two, another than the first, and SQLite gives `no such column` for the first in the order of the
// text; each is a quarter as likely in a profile without the indexes among [Profile.Faults], as
// [Sequence.fault] says. The columns are kept for the conditions of a WHERE, as
// [Sequence.indexConditions] draws them, only as [Sequence.indexed] says: if the table is live, the
// name is not that of a live table or of an index that the sequence keeps, and every column is one of
// the shape.
func (q *Sequence) createIndex(index, table stmt.Name) []stmt.IndexColumn {
	r := q.rIndexes
	shape := q.shapeOf(table)
	order := make([]int, len(shape))
	for i := range order {
		order[i] = i
	}
	shuffle(r, order)
	if q.profile.Indexes > 1 && len(order) > 1 && r.IntN(4) != 0 {
		// Every column but the key, whose pin the rules read by a get, so that a WHERE pins several
		// columns of the index, and reaches the key after them.
		order = slices.DeleteFunc(order, func(position int) bool { return shape[position].PrimaryKey })
	} else {
		order = order[:1+r.IntN(len(order))]
	}
	var columns, kept []stmt.IndexColumn
	for _, position := range order {
		c := shape[position]
		descending := r.IntN(3) == 0
		columns = append(columns, stmt.IndexColumn{Name: stmt.Name{Raw: q.spell(r, c.Name.Value)}, Descending: descending})
		kept = append(kept, stmt.IndexColumn{Name: stmt.Name{Value: c.Name.Value}, Descending: descending})
	}
	missing := ""
	for range map[bool]int{true: 1, false: 0}[q.fault(r, areaIndexes, 12)] + map[bool]int{true: 1, false: 0}[q.fault(r, areaIndexes, 40)] {
		// A second name is another than the first, since a column named twice is not supported.
		raw := q.missing(r, shape)
		if missing != "" && nameOf(raw, 0).Fold() == nameOf(missing, 0).Fold() {
			break
		}
		missing = raw
		at := r.IntN(len(columns) + 1)
		columns = slices.Insert(columns, at, stmt.IndexColumn{Name: stmt.Name{Raw: raw}, Descending: r.IntN(3) == 0})
	}
	if q.live[table.Fold()] && !q.live[index.Fold()] && !q.liveIndex(index.Fold()) && missing == "" {
		indexed := q.indexed[table.Fold()]
		if len(indexed) == storedIndexes {
			indexed = indexed[1:]
		}
		q.indexed[table.Fold()] = append(slices.Clip(indexed), sequenceIndex{name: index.Fold(), columns: kept})
	}
	return columns
}

// indexConditions of a WHERE of a statement of table, which an index of the sequence on table can
// serve, or nil if the sequence keeps no index of table, as [Sequence.indexed] says: for the columns
// of one of them, the leading ones, from one to all, or with [Profile.Indexes] of 2 all of them one
// time in two, each pinned by = with a value as [Sequence.conditionValue] draws it, or by IS NULL one
// time in three for a column that takes NULL and one time in eight otherwise. The key of the index is
// its columns, and the key of the table after them if it is not among them. With [Profile.Indexes] of
// 2, one time in four, the next column of that key after the pins, if it has one, is pinned by = too;
// otherwise, one time in two, that column has a bound or two, each open or closed. One time in eight
// the WHERE has no pin, and a bound or two on the first column of the index. Then, one time in three,
// it has one more condition, one of those that [Sequence.conditions] draws. The conditions are in any
// order one time in three.
func (q *Sequence) indexConditions(table stmt.Name) []stmt.Condition {
	r := q.rIndexes
	indexed := q.indexed[table.Fold()]
	if len(indexed) == 0 {
		return nil
	}
	shape := q.shapeOf(table)
	columns := indexed[r.IntN(len(indexed))].columns
	var key []string
	for _, c := range columns {
		key = append(key, c.Name.Value)
	}
	for _, c := range shape {
		if c.PrimaryKey && !slices.Contains(key, c.Name.Value) {
			key = append(key, c.Name.Value)
		}
	}
	typeOf := func(fold string) stmt.Type {
		if c := columnOfShape(shape, fold); c != nil {
			return c.Type
		}
		return anyType(r)
	}
	pins := 1 + r.IntN(len(columns))
	if q.profile.Indexes > 1 && r.IntN(2) == 0 {
		pins = len(columns)
	}
	if r.IntN(8) == 0 {
		// No pin, and a bound or two on the first column of the index.
		pins = 0
	}
	var cs []stmt.Condition
	for _, fold := range key[:pins] {
		c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, fold)}, Operator: stmt.OpEqual}
		// A column that takes NULL is pinned by IS NULL more often, since only there does the pin find
		// rows, and the path of the index pins NULL.
		odds := 8
		if column := columnOfShape(shape, fold); column != nil && !column.PrimaryKey && !column.NotNull {
			odds = 3
		}
		if r.IntN(odds) == 0 {
			c.Operator = stmt.OpIsNull
		} else {
			c.Value.Value = q.conditionValue(r, areaIndexes, table, fold, typeOf(fold))
		}
		cs = append(cs, c)
	}
	if pins > 0 && pins < len(key) && q.profile.Indexes > 1 && r.IntN(4) == 0 {
		// The next column of the key of the index pinned too, so that a WHERE pins the key of the
		// table after every column of an index.
		fold := key[pins]
		c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, fold)}, Operator: stmt.OpEqual}
		c.Value.Value = q.conditionValue(r, areaIndexes, table, fold, typeOf(fold))
		cs = append(cs, c)
	} else if pins < len(key) && (pins == 0 || r.IntN(2) == 0) {
		fold := key[pins]
		bounds := []stmt.Operator{stmt.OpLess, stmt.OpLessOrEqual, stmt.OpGreater, stmt.OpGreaterOrEqual}
		for range 1 + r.IntN(2) {
			c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, fold)}, Operator: bounds[r.IntN(len(bounds))]}
			c.Value.Value = q.conditionValue(r, areaIndexes, table, fold, typeOf(fold))
			cs = append(cs, c)
		}
	}
	if r.IntN(3) == 0 {
		more := q.conditions(r, areaIndexes, table)
		cs = append(cs, more[r.IntN(len(more))])
	}
	if r.IntN(3) == 0 {
		shuffle(r, cs)
	}
	return cs
}

// shuffle the elements of s into an order at random.
func shuffle[T any](r Rand, s []T) {
	for i := len(s) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		s[i], s[j] = s[j], s[i]
	}
}

// columnOfShape of the fold of a name: the column of the shape of that name, or nil.
func columnOfShape(shape []stmt.Column, fold string) *stmt.Column {
	for i := range shape {
		if shape[i].Name.Value == fold {
			return &shape[i]
		}
	}
	return nil
}

// missing gives the raw text of a name of a column of the sequence that the shape does not have.
// The sequence has more names than a shape has columns, so there is one.
func (q *Sequence) missing(r Rand, shape []stmt.Column) string {
	var raws []string
	for _, raw := range q.columns {
		if columnOfShape(shape, nameOf(raw, 0).Fold()) == nil {
			raws = append(raws, raw)
		}
	}
	return raws[r.IntN(len(raws))]
}

// update gives the assignments and the conditions of an update of table, as [Generator.Update] gives
// them: from one to all of the columns of its shape, each once, in any order, where one time in two
// the key is left out of two or more; one update in twelve has a name that the shape does not have
// in place of one of them, which can be in double quotes, or one in forty-eight in a profile without
// the changes among [Profile.Faults], as [Sequence.fault] says. The value of each is NULL one time in
// ten, also for the key and for a column that does not take NULL, for a real one of the two zeros,
// 0.0 and -0.0, one time in four of the others, for text or a blob one of about [LengthLimit] bytes
// one time in twelve of the others, and otherwise drawn as
// [Sequence.conditionValue] draws a value for a condition on its column: mostly a value that an
// insert gave the column, a neighbour of one, or an edge of its type, now and then one of another
// type, and as an insert gets one otherwise, so that a change of the key meets a key that exists and
// also one that does not. Three updates in four have a WHERE, as [Sequence.changeConditions] draws one. The
// values of the assignments are kept, as [Sequence.store] keeps those of an insert.
func (q *Sequence) update(table stmt.Name) ([]stmt.Assignment, []stmt.Condition) {
	r := q.rChanges
	shape := q.shapeOf(table)
	order := make([]int, len(shape))
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	order = order[:1+r.IntN(len(order))]
	if len(order) > 1 && r.IntN(2) == 0 {
		order = slices.DeleteFunc(order, func(position int) bool { return shape[position].PrimaryKey })
	}
	missing := -1
	if q.fault(r, areaChanges, 12) {
		missing = r.IntN(len(order))
	}
	var set []stmt.Assignment
	var list []string
	var values []stmt.Value
	for i, position := range order {
		c := shape[position]
		raw, fold, t := q.spell(r, c.Name.Value), c.Name.Value, c.Type
		if i == missing {
			raw, fold, t = q.missing(r, shape), "", anyType(r)
		}
		var v stmt.Value
		switch {
		case r.IntN(10) == 0:
			v = stmt.Null{}
		case t == stmt.TypeReal && r.IntN(4) == 0:
			// The two zeros, which SQLite takes for one key and stores as they are written.
			v = stmt.Real([]float64{0, math.Copysign(0, -1)}[r.IntN(2)])
		case (t == stmt.TypeText || t == stmt.TypeBlob) && r.IntN(12) == 0:
			// Of about the length limit, so that the record of the row is too long now and then.
			s := long(r, func(n int) string { return runes(r, n) })
			v = map[bool]stmt.Value{true: stmt.Text(s), false: stmt.Blob(s)}[t == stmt.TypeText]
		default:
			v = q.conditionValue(r, areaChanges, table, fold, t)
		}
		set = append(set, stmt.Assignment{Column: stmt.Name{Raw: raw}, Value: stmt.Term{Value: v}})
		list, values = append(list, raw), append(values, v)
	}
	q.store(table, shape, list, values)
	if r.IntN(4) == 0 {
		return set, nil
	}
	return set, q.changeConditions(table)
}

// delete gives the conditions of a delete from table, as [Generator.Delete] gives them: none one
// time in five, which deletes every row, and otherwise a WHERE as [Sequence.changeConditions] draws
// one.
func (q *Sequence) delete(table stmt.Name) []stmt.Condition {
	if q.rChanges.IntN(5) == 0 {
		return nil
	}
	return q.changeConditions(table)
}

// changeConditions of the WHERE of an update or a delete of table: one time in two, if the sequence
// keeps a value of the key, the one condition on the key with the i-th of the values that the sequence
// keeps for it, which finds the row of the key if it is there, since most WHERE that
// [Sequence.conditions] draws find no row of a table of a few rows. The condition is that the key is
// the value, or, for every third i from the third on, that the key is on one side of it, with <,
// <=, >, and >= in turn, which finds the rows on that side; the operator comes from i, so that the
// sequence draws the same random numbers as for the key equal to the value. Otherwise the
// conditions are those that [Sequence.indexConditions] draws, where [Sequence.indexWhere] says so,
// and else as [Sequence.conditions] draws them.
func (q *Sequence) changeConditions(table stmt.Name) []stmt.Condition {
	r := q.rChanges
	shape := q.shapeOf(table)
	key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	if stored := q.stored[[2]string{table.Fold(), key.Name.Value}]; len(stored) > 0 && r.IntN(2) == 0 {
		// The spelling first, and then the value, as for the key equal to the value.
		column := stmt.Name{Raw: q.spell(r, key.Name.Value)}
		i := r.IntN(len(stored))
		op := stmt.OpEqual
		if i%3 == 2 {
			op = bounds[i/3%len(bounds)]
		}
		return []stmt.Condition{{Column: column, Operator: op, Value: stmt.Term{Value: stored[i]}}}
	}
	if q.indexWhere(table) {
		return q.indexConditions(table)
	}
	return q.conditions(r, areaChanges, table)
}

// conditionValue for a condition on the column of the fold of a select of table, of type t, or of a
// type at random for "", so that a condition is often true of some rows and false of others: NULL one
// time in twenty; a value of another type than t one time in twenty; a value that an insert of the
// sequence gave the column, as [Sequence.store] keeps them, two times in five, and a neighbour of
// one, as [Sequence.neighbour] gives it, one time in eight; an edge of t, as [edgeOf] gives it, one
// time in ten; and otherwise as [Sequence.valueOf] gives it. In a profile without the area among
// [Profile.Faults], NULL and a value of another type come a quarter as often, as [Sequence.fault]
// says, and a value as [Sequence.valueOf] gives it in their place.
func (q *Sequence) conditionValue(r Rand, area string, table stmt.Name, fold string, t stmt.Type) stmt.Value {
	stored := q.stored[[2]string{table.Fold(), fold}]
	n := r.IntN(40)
	if n < 4 && !q.fault(r, area, 1) {
		// In a profile without the faults of the area, three of four of the values that would be NULL
		// or of another type are as [Sequence.valueOf] gives them.
		n = 39
	}
	switch {
	case n < 2:
		return stmt.Null{}
	case n < 4:
		for {
			if other := anyType(r); kindOf(other) != kindOf(t) {
				return q.valueOf(r, other)
			}
		}
	case n < 20 && len(stored) > 0:
		return stored[r.IntN(len(stored))]
	case n < 25 && len(stored) > 0:
		return neighbour(r, stored[r.IntN(len(stored))])
	case n < 29:
		return edgeOf(r, t)
	}
	return q.valueOf(r, t)
}

// kindOf the values of a column of the type t: INT and INTEGER hold one kind.
func kindOf(t stmt.Type) stmt.Type {
	if t == stmt.TypeInteger {
		return stmt.TypeInt
	}
	return t
}

// edgeOf the type t at random from r: for an integer the limits of 64 bits, 0, -1, and 1; for a real
// 0.0, -0.0, the largest and the smallest finite reals of each sign; for text the empty text, the
// greatest code point, the least that a text holds, and a text of a byte more than [LengthLimit]; and
// for a blob the empty blob, one zero byte, one byte of 255, and a blob of a byte more than
// [LengthLimit].
func edgeOf(r Rand, t stmt.Type) stmt.Value {
	switch t {
	case stmt.TypeReal:
		return stmt.Real([]float64{0, math.Copysign(0, -1), math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64}[r.IntN(6)])
	case stmt.TypeText:
		return stmt.Text([]string{"", "\U0010ffff", "\x01", strings.Repeat("e", LengthLimit+1)}[r.IntN(4)])
	case stmt.TypeBlob:
		return stmt.Blob([][]byte{{}, {0}, {0xff}, []byte(strings.Repeat("e", LengthLimit+1))}[r.IntN(4)])
	}
	return stmt.Integer([]int64{math.MinInt64, math.MaxInt64, 0, -1, 1}[r.IntN(5)])
}

// neighbour of the value v, of its type: the next integer above or below, the next real above or
// below, or for text and a blob a prefix of it, the least value above it, or the value with its last
// character or byte one more or one less, whichever of these stays a value of its type, and v where
// none does.
func neighbour(r Rand, v stmt.Value) stmt.Value {
	up := r.IntN(2) == 0
	switch v := v.(type) {
	case stmt.Integer:
		switch {
		case up && v < math.MaxInt64, v == math.MinInt64:
			return v + 1
		default:
			return v - 1
		}
	case stmt.Real:
		f := math.Nextafter(float64(v), math.Inf(map[bool]int{true: 1, false: -1}[up]))
		if math.IsInf(f, 0) {
			return v
		}
		return stmt.Real(f)
	case stmt.Text:
		runes := []rune(string(v))
		switch n := r.IntN(3); {
		case n == 0 && len(runes) > 0:
			return stmt.Text(string(runes[:r.IntN(len(runes))]))
		case n == 1 || len(runes) == 0:
			// The least text above v: v and the least character that a text holds.
			return v + "\x01"
		}
		last := &runes[len(runes)-1]
		if c := *last + rune(map[bool]int{true: 1, false: -1}[up]); c > 0 && c <= unicode.MaxRune && (c < 0xd800 || c > 0xdfff) {
			*last = c
		}
		return stmt.Text(string(runes))
	case stmt.Blob:
		switch n := r.IntN(3); {
		case n == 0 && len(v) > 0:
			return slices.Clone(v[:r.IntN(len(v))])
		case n == 1 || len(v) == 0:
			// The least blob above v: v and a zero byte.
			return append(slices.Clone(v), 0)
		}
		b := slices.Clone(v)
		switch {
		case up && b[len(b)-1] < 0xff:
			b[len(b)-1]++
		case !up && b[len(b)-1] > 0:
			b[len(b)-1]--
		}
		return b
	}
	return v
}

// valueOf the type t: mostly from a small set, so that keys repeat, and otherwise as [ValueOf]
// gives it, with weight on its edges, or, for text and a blob, one time in 16 of about
// [LengthLimit] bytes.
func (q *Sequence) valueOf(r Rand, t stmt.Type) stmt.Value {
	if t == stmt.TypeText || t == stmt.TypeBlob {
		if r.IntN(16) == 0 {
			v := long(r, func(n int) string { return runes(r, n) })
			if t == stmt.TypeBlob {
				return stmt.Blob(v)
			}
			return stmt.Text(v)
		}
	}
	if r.IntN(4) == 0 {
		return ValueOf(r, t)
	}
	switch t {
	case stmt.TypeReal:
		return stmt.Real([]float64{0, math.Copysign(0, -1), 1, 2, 0.5, -1.5}[r.IntN(6)])
	case stmt.TypeText:
		return stmt.Text([]string{"", "a", "A", "it's", "é"}[r.IntN(5)])
	case stmt.TypeBlob:
		return stmt.Blob([][]byte{{}, {0}, {0, 0}, {'a'}}[r.IntN(4)])
	}
	return stmt.Integer(int64(r.IntN(5)) - 1)
}

// anyValue of a type at random, as an argument.
func (q *Sequence) anyValue(r Rand) any {
	return Argument(q.valueOf(r, anyType(r)))
}

// Like a, an argument: another value of the sequence of the same Go type, if a is a value that a
// column holds of type int64, float64, string, or []byte, and otherwise a, so that an argument that
// no column holds stays one.
func (q *Sequence) Like(a any) any {
	if s, ok := a.(string); ok && (!utf8.ValidString(s) || strings.ContainsRune(s, 0)) {
		return a
	}
	r := q.rArguments
	switch v := a.(type) {
	case int64:
		return Argument(q.valueOf(r, stmt.TypeInt))
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return Argument(q.valueOf(r, stmt.TypeReal))
		}
	case string:
		return Argument(q.valueOf(r, stmt.TypeText))
	case []byte:
		if v != nil {
			return Argument(q.valueOf(r, stmt.TypeBlob))
		}
	}
	return a
}

// Others are texts other than the statements that the generator of a sequence writes, with %[1]v for
// a table, %[2]v for a column, and %[3]v for a small integer: statements that the pass does not
// support, among them literals that SQLite reads as an infinite real or as a real for an integer
// outside 64 bits, texts that SQLite refuses with a syntax error, and texts without a statement.
// Eight are statements that the pass supports, which a sequence does not mark as [Call.Supported], as
// it marks none of the texts of Others: an insert without a list of columns and a select of a column,
// of slice 5, an insert of NULL, of slice 6, two selects with WHERE, of slice 7, an update, of slice
// 8, and a create table with IF NOT EXISTS and a drop table with IF EXISTS, of slice 10.
var Others = []string{
	"create table %[1]v (%[2]v any primary key) strict, without rowid",
	"create table %[1]v (%[2]v varchar primary key) strict, without rowid",
	"insert into %[1]v (%[2]v) values (1e999)",
	"insert into %[1]v (%[2]v) values (-1e400)",
	"insert into %[1]v (%[2]v) values (9223372036854775808)",
	"insert into %[1]v (%[2]v) values (0x%[3]v)",
	"insert into %[1]v (%[2]v) values (null)",
	"insert into %[1]v (%[2]v) values (-'a')",
	"insert into %[1]v (%[2]v) values (x'6')",
	"insert into %[1]v (%[2]v) values ('a''b)",
	"create table %[1]v (%[2]v int primary key) strict",
	"create table if not exists %[1]v (%[2]v int primary key) strict, without rowid",
	`create table "" (%[2]v int primary key) strict, without rowid`,
	`create table %[1]v ("" int primary key) strict, without rowid`,
	"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
	"insert into %[1]v (%[2]v) values (?), (?)",
	"insert into %[1]v (%[2]v) values (?1)",
	"insert into %[1]v (%[2]v) values (:a)",
	"insert into %[1]v (%[2]v) values (@a)",
	"insert into %[1]v (%[2]v) values ($a)",
	"insert into %[1]v (%[2]v) values (-?)",
	"insert into %[1]v (%[2]v) values (? ?)",
	"select * from %[1]v where %[2]v = ?",
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

// Call of a text: what an application gives to database/sql.
type Call struct {
	Src string
	// Args of the call, by position: an int64, a float64, a string, a []byte that is not nil, or nil
	// for NULL for the arguments that s4ql supports, as [Argument] gives them, and other values, such
	// as a [sql.NamedArg], an int32, a float32, or one of [Unsupported], for the cases around them.
	Args []any
	// Query reports whether to run the call as a query.
	Query bool
	// Supported reports whether the text is a statement of the slices that a [Sequence] writes, which
	// s4ql supports.
	Supported bool
}

// Integers of args, and true, if each is a Go integer without a name that fits a signed 64-bit
// integer, as database/sql hands it to the driver as an int64.
func Integers(args []any) ([]int64, bool) {
	vs := make([]int64, 0, len(args))
	for _, a := range args {
		v, ok := integer(a)
		if !ok {
			return nil, false
		}
		vs = append(vs, v)
	}
	return vs, true
}

// integer of the argument a, and true, if it is a Go integer that fits a signed 64-bit integer.
func integer(a any) (int64, bool) {
	rv := reflect.ValueOf(a)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if rv.Uint() > math.MaxInt64 {
			return 0, false
		}
		return int64(rv.Uint()), true
	}
	return 0, false
}

// Next call of the sequence. Its arguments are those of its parameters, and [Sequence.around]
// changes them for one call in [Profile.Around][0] of a statement with a parameter and for one in
// [Profile.Around][1] of a statement without one.
func (q *Sequence) Next() Call {
	q.step++
	c := q.next()
	if odds := q.profile.Around[map[bool]int{true: 0, false: 1}[len(c.Args) > 0]]; c.Supported && odds > 0 && q.rArguments.IntN(odds) == 0 {
		c.Args = q.around(c.Args)
	}
	return c
}

// aim gives the generator its pools of names for the statement of the kind k, and the kind of the
// statement. If aimed, the statement is aimed at the tables as the sequence left them, by its own
// statements, whether SQLite took them or not: a create table names a table that the sequence has not
// created, or has dropped since, if there is one; every other statement names a table that it has
// created and not dropped, if there is one; and if there is none, the statement is a create table in
// its place, but for a drop table and a drop index; a create index names an index that the sequence
// does not keep, or one time in four one that it keeps, and a drop index one that it keeps, as
// [Sequence.indexed] says, if there is one, and one drop table in four names an index that it keeps. So most statements of a profile with
// [Profile.Aim] meet a table that is there, where most of a sequence that names its tables at random
// do not. Otherwise the generator has every name of the sequence for each position.
func (q *Sequence) aim(r Rand, k Kind, aimed bool) Kind {
	copy(q.g.pools, q.pools)
	if !aimed {
		return k
	}
	var live, dead []string
	for _, raw := range q.tables {
		if q.live[nameOf(raw, 0).Fold()] {
			live = append(live, raw)
		} else {
			dead = append(dead, raw)
		}
	}
	if len(live) == 0 && k != KindDropTable && k != KindDropIndex {
		k = KindCreateTable
	}
	if len(live) > 0 {
		for _, position := range []Position{InsertTable, SelectTable, UpdateTable, DeleteTable, IndexTable, DropTable} {
			q.g.pools[position] = live
		}
	}
	if len(dead) > 0 {
		q.g.pools[CreateTable] = dead
	}
	// A create index names an index that the sequence does not keep, and a drop index one that it
	// keeps, if there is one.
	live, dead = nil, nil
	for _, raw := range q.indexes {
		if q.liveIndex(nameOf(raw, 0).Fold()) {
			live = append(live, raw)
		} else {
			dead = append(dead, raw)
		}
	}
	if len(live) > 0 {
		q.g.pools[DropIndex] = live
		if k == KindDropTable && r.IntN(4) == 0 {
			// One drop table in four names an index that is there, as a table and an index meet at a name
			// in the pools of the sequence.
			q.g.pools[DropTable] = live
		}
	}
	switch {
	case k == KindCreateIndex && len(live) > 0 && r.IntN(4) == 0:
		// One create index in four names an index that the sequence keeps, which SQLite refuses, or
		// leaves alone with IF NOT EXISTS.
		q.g.pools[IndexName] = live
	case len(dead) > 0:
		q.g.pools[IndexName] = dead
	}
	return k
}

// liveIndex reports whether the sequence keeps an index of the fold of a name, as
// [Sequence.indexed] says.
func (q *Sequence) liveIndex(fold string) bool {
	for _, indexed := range q.indexed {
		if slices.ContainsFunc(indexed, func(ix sequenceIndex) bool { return ix.name == fold }) {
			return true
		}
	}
	return false
}

// fault reports whether a statement of the area has a fault at random from r, whose odds are one
// in n in a profile that has the faults of the area, as [Profile.Faults] says, and a quarter of that
// in another, so that most statements beside those that a profile weighs do their work.
func (q *Sequence) fault(r Rand, area string, n int) bool {
	if !slices.Contains(q.profile.Faults, area) {
		n *= 4
	}
	return r.IntN(n) == 0
}

// around args, the arguments of a call, are arguments of the cases around them. For a statement
// without a parameter, they are one argument, a value or one that no column holds. For one with a
// parameter, the arguments lack the last, have one more, have one that no column holds, have one
// with a name, or have a value of another Go type that s4ql supports: an integer of another Go type
// than int64, a float32 for a float64, and for text or a blob one that no column holds of the same
// Go type, and for NULL a nil []byte, which no column holds either.
func (q *Sequence) around(args []any) []any {
	r := q.rArguments
	if len(args) == 0 {
		if r.IntN(2) == 0 {
			return []any{Unsupported[r.IntN(len(Unsupported))]}
		}
		return []any{q.anyValue(r)}
	}
	args = slices.Clone(args)
	at := r.IntN(len(args))
	switch r.IntN(5) {
	case 0:
		return args[:len(args)-1]
	case 1:
		return append(args, q.anyValue(r))
	case 2:
		args[at] = Unsupported[r.IntN(len(Unsupported))]
	case 3:
		args[at] = sql.Named("v", args[at])
	default:
		switch v := args[at].(type) {
		case int64:
			switch {
			case v >= 0 && r.IntN(2) == 0:
				args[at] = uint64(v)
			case v >= math.MinInt32 && r.IntN(2) == 0:
				args[at] = int32(v)
			default:
				args[at] = int(v)
			}
		case float64:
			// Only where a float32 holds it as a finite real, so that it stays a value.
			if f := float32(v); !math.IsInf(float64(f), 0) {
				args[at] = f
			}
		case string:
			args[at] = []string{"a\x00b", "\xff", v + "\x00"}[r.IntN(3)]
		case []byte, nil:
			// A nil []byte, which does not say whether it is NULL or the empty blob.
			args[at] = []byte(nil)
		}
	}
	return args
}

// next call of the sequence, with an argument for each parameter of a statement of the
// slices that a sequence writes, and for each ? of a text that is not.
func (q *Sequence) next() Call {
	p := q.profile
	if p.Others > 0 && q.rOthers.IntN(p.Others) == 0 {
		r := q.rOthers
		form := Others[r.IntN(len(Others))]
		table, column, value := q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))], r.IntN(5)
		c := Call{Src: form, Query: r.IntN(2) == 0}
		// Sprintf would add the arguments that a form without a verb does not use to its text.
		if strings.Contains(form, "%") {
			c.Src = fmt.Sprintf(form, table, column, value)
		}
		for range strings.Count(c.Src, "?") {
			c.Args = append(c.Args, q.anyValue(r))
		}
		return c
	}
	r := q.rPlan
	total := 0
	for _, w := range p.Weights {
		total += w
	}
	k, n := KindCreateTable, r.IntN(total)
	for n >= p.Weights[k] {
		n -= p.Weights[k]
		k++
	}
	k = q.aim(r, k, p.Aim > 0 && r.IntN(4) < p.Aim)
	src, s, args := q.g.CallOf(k)
	switch s := s.(type) {
	case *stmt.CreateTable:
		q.live[s.Table.Fold()] = true
	case *stmt.DropTable:
		delete(q.live, s.Table.Fold())
		delete(q.indexed, s.Table.Fold())
	case *stmt.DropIndex:
		for table, indexed := range q.indexed {
			q.indexed[table] = slices.DeleteFunc(indexed, func(ix sequenceIndex) bool { return ix.name == s.Index.Fold() })
		}
	}
	// A query runs through Exec one time in eight, and every other statement through Query one time
	// in eight.
	return Call{Src: src, Args: args, Query: (k == KindSelect) != (r.IntN(8) == 0), Supported: true}
}
