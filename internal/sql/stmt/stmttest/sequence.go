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
	rPlan, rTables, rInserts, rSelects, rChanges, rIndexes, rDefaults, rArguments, rOthers, rOrder, rReturning, rSubquery, rArithmetic, rSeveral Rand
	g                                                                                                                                            *Generator
	// inner are the selects of the subqueries that the hooks of the statement that the sequence writes
	// gave, for [Generator.Inner], by their numbers from 1.
	inner []stmt.Select
	// decided is set by [Sequence.Decide].
	decided bool
	// received is set by [Sequence.receive] for the RETURNING of its update, which goqite's receive
	// has, and by [Sequence.several] for the last statement of some of its texts, and
	// [Sequence.returning] clears it.
	received bool
	// failingRow is set by [Sequence.arithmeticCondition] for a select whose WHERE has a value that
	// fails for some rows, for the LIMIT that [Sequence.order] then gives it, and the hook of the WHERE
	// clears it first.
	failingRow bool
	// again is the last call of a select, an update, or a delete with a subquery, for
	// [Profile.Again], or nil, and limits are the numbers from 1 of the parameters of its LIMITs, of
	// the select and of its subqueries.
	again  *Call
	limits []int
	// selected are the conditions of the WHERE of the select that the sequence writes, as
	// [Sequence.where] gave them, for its ORDER BY.
	selected []stmt.Condition
	// aimed is the ORDER BY that [Sequence.aimedWhere] drew together with the WHERE of the select that
	// the sequence writes, or nil, and aimedLimit whether the select has a LIMIT for it.
	aimed      []orderColumn
	aimedLimit bool
	tables     []string
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
	// since then no drop table, whether SQLite took the statement or not. The texts of [Others] are
	// not followed, of which one creates a table and one drops one.
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

// Stream of random numbers of the name, for what runs the sequence and draws for itself: the same
// numbers for the same seed and name, whatever the
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
	q.rOrder, q.rReturning, q.rSubquery, q.rArithmetic = q.stream(areaOrder), q.stream(areaReturning), q.stream(areaSubquery), q.stream(areaArithmetic)
	q.rSeveral = q.stream(areaSeveral)
	r := q.entity(areaNames, "")
	tableNames, columnNames := common[0], common[1]
	for range max(1+r.IntN(2), p.TableNames) {
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
	if p.on(areaOrder) {
		q.g.Order = q.order
	}
	if p.on(areaReturning) {
		q.g.Return = q.returning
	}
	if p.on(areaSubquery) {
		q.g.Inner = func(n int) stmt.Select { return q.inner[n-1] }
	}
	if p.on(areaArithmetic) {
		q.arithmeticHooks()
	}
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
		// With [Profile.DefaultFirst], a default on the first column and none on the last.
		first := q.profile.DefaultFirst > 0 && len(shape) > 1 && d.IntN(q.profile.DefaultFirst) == 0
		for i := range shape {
			switch {
			case first && i == 0, first && i < len(shape)-1 && d.IntN(2) == 0, !first && (defaults == 1 || defaults == 2 && d.IntN(2) == 0):
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
			case c.PrimaryKey && q.profile.KeyLeftOut > 0 && r.IntN(q.profile.KeyLeftOut) == 0:
				return true
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
		if c.PrimaryKey && q.profile.Keys > 0 && r.IntN(q.profile.Keys) == 0 {
			values = append(values, keyOfStep(c.Type, q.step))
			continue
		}
		if !c.PrimaryKey && q.profile.Ties > 0 && r.IntN(q.profile.Ties) == 0 {
			values = append(values, tieValue(c.Type, r.IntN(2)))
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

// keyOfStep is a value of a key of the type t for the statement number step, which no other insert
// of [keyOfStep] gives, and [Sequence.valueOf] gives only now and then, by [ValueOf], so that an
// insert seldom meets a key that exists.
func keyOfStep(t stmt.Type, step uint64) stmt.Value {
	switch t {
	case stmt.TypeReal:
		return stmt.Real(float64(step) + 0.25)
	case stmt.TypeText:
		return stmt.Text(fmt.Sprint("k", step))
	case stmt.TypeBlob:
		return stmt.Blob(fmt.Sprint("k", step))
	}
	return stmt.Integer(100 + int64(step))
}

// tieValue is the value of the type t of the number n, 0 or 1, of the two values of [Profile.Ties].
func tieValue(t stmt.Type, n int) stmt.Value {
	switch t {
	case stmt.TypeReal:
		return stmt.Real(float64(n) + 0.5)
	case stmt.TypeText:
		return stmt.Text([]string{"a", "b"}[n])
	case stmt.TypeBlob:
		return stmt.Blob([]string{"a", "b"}[n])
	}
	return stmt.Integer(int64(n))
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

// where gives the conditions of the WHERE of a select of table, as [Generator.Where] gives them: for
// one select in [Profile.Aimed] of a table of which the sequence keeps an index, those that
// [Sequence.aimedWhere] draws together with its ORDER BY. Otherwise none for all but [Profile.Where]
// selects in four. Otherwise, where [Sequence.indexWhere] says so,
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
	q.selected = q.subqueries(table, q.selectWhere(table), false)
	if q.profile.SubqueryLimit > 0 && slices.ContainsFunc(q.selected, func(c stmt.Condition) bool { return c.Value.Subquery > 0 }) && q.rSubquery.IntN(2) == 0 {
		q.giving(table, q.selected)
	}
	return q.selected
}

// giving makes the first comparison of cs, the conditions of a select of table, with a subquery of
// table of a named column one that the rows of table meet whenever the subquery finds a value that is
// not NULL: the comparison is by =, the subquery gives the column that it is compared with, and its
// LIMIT 0 becomes 1. A comparison of a name that the shape does not have, or with a subquery of
// another table or of *, is left as it is, and so are the others.
func (q *Sequence) giving(table stmt.Name, cs []stmt.Condition) {
	shape := q.shapeOf(table)
	for i, c := range cs {
		if c.Value.Subquery == 0 {
			continue
		}
		sub := &q.inner[c.Value.Subquery-1]
		column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
		if column == nil || sub.Columns == nil || nameOf(sub.Table.Raw, 0).Fold() != table.Fold() {
			return
		}
		cs[i].Operator = stmt.OpEqual
		sub.Columns = []stmt.Name{{Raw: q.spell(q.rSubquery, column.Name.Value)}}
		if sub.Limit.Value == stmt.Value(stmt.Integer(0)) {
			sub.Limit.Value = stmt.Integer(1)
		}
		return
	}
}

// selectWhere gives the conditions of the WHERE of a select of table without its subqueries, as
// [Sequence.where] says.
func (q *Sequence) selectWhere(table stmt.Name) []stmt.Condition {
	q.selected, q.aimed, q.aimedLimit = nil, nil, false
	if p := q.profile; p.Aimed > 0 && len(q.indexed[table.Fold()]) > 0 && q.rOrder.IntN(p.Aimed) == 0 {
		q.selected = q.aimedWhere(q.rOrder, table)
		return q.selected
	}
	if q.rSelects.IntN(4) >= q.profile.Where {
		return nil
	}
	if q.indexWhere(table) {
		q.selected = q.indexConditions(q.rIndexes, table)
	} else {
		q.selected = q.conditions(q.rSelects, areaSelects, table)
	}
	return q.selected
}

// order gives the ORDER BY and the LIMIT of a select of table, as [Generator.Order] gives them, after
// the WHERE of the select: an ORDER BY for [Profile.Order] selects in four, as
// [Sequence.orderColumns] draws it, and a LIMIT for one select in [Profile.Limit], and for every
// select whose ORDER BY is aimed at an index beside one of a range: 0 one time in eight, from 1 to 4
// one time in two, from 6 to 105 one time in four, which is mostly above the number of rows of a
// table of the sequence, and else the largest integer of 64 bits. A select without a LIMIT whose
// WHERE [Sequence.arithmeticCondition] or [Sequence.failingWhere] gave a value that fails for some
// rows gets one of 1 or 2 three times in four. With [Profile.SubqueryLimit], a select with subqueries and without a LIMIT then gets
// one of 1 or from 2 to 4.
func (q *Sequence) order(table stmt.Name) ([]stmt.OrderColumn, stmt.Term) {
	r, p := q.rOrder, q.profile
	var columns []stmt.OrderColumn
	aimed := false
	switch {
	case q.aimed != nil:
		for _, c := range q.aimed {
			columns = append(columns, stmt.OrderColumn{Name: stmt.Name{Raw: q.spell(r, c.fold)}, Descending: c.descending})
		}
		aimed = q.aimedLimit
	case r.IntN(4) < p.Order:
		columns, aimed = q.orderColumns(r, table)
	}
	var limit stmt.Term
	if p.Limit > 0 && r.IntN(p.Limit) == 0 || aimed {
		switch n := r.IntN(8); {
		case n == 0:
			limit.Value = stmt.Integer(0)
		case n < 5:
			limit.Value = stmt.Integer(1 + r.IntN(4))
		case n < 7:
			limit.Value = stmt.Integer(6 + r.IntN(100))
		default:
			limit.Value = stmt.Integer(math.MaxInt64)
		}
	}
	if q.failingRow && limit.Value == nil && r.IntN(4) != 0 {
		// A LIMIT for a select whose WHERE fails for some rows, so that the rows that it reads, and the
		// direction that the engine draws, decide whether it fails.
		limit.Value = stmt.Integer(1 + r.IntN(2))
	}
	if p.SubqueryLimit > 0 && limit.Value == nil && slices.ContainsFunc(q.selected, func(c stmt.Condition) bool { return c.Value.Subquery > 0 }) && r.IntN(p.SubqueryLimit) == 0 {
		limit.Value = stmt.Integer(1)
		if r.IntN(2) == 0 {
			limit.Value = stmt.Integer(2 + r.IntN(3))
		}
	}
	return columns, limit
}

// returning gives the RETURNING of an insert, an update, or a delete of table, as [Generator.Return]
// gives it: one for one statement in [Profile.Returning], and for each goqite's receive that
// [Sequence.receive] wrote. It is * one time in three, and otherwise
// from one to [sequenceColumns]+1 names of the columns of the shape of the table, of which one in
// four after the first is an earlier one, which the clause then names twice, and one in twelve any
// name of a column of the sequence, which the table may not have: a fault of the area, a quarter as
// likely in a profile without it among [Profile.Faults], as [Sequence.fault] says.
func (q *Sequence) returning(table stmt.Name) (bool, []string) {
	r := q.rReturning
	received := q.received
	q.received = false
	if r.IntN(q.profile.Returning) != 0 && !received {
		return false, nil
	}
	if r.IntN(3) == 0 {
		return true, nil
	}
	shape := q.shapeOf(table)
	var names []string
	for i := range 1 + r.IntN(sequenceColumns+1) {
		switch {
		case i > 0 && r.IntN(4) == 0:
			names = append(names, names[r.IntN(i)])
		case q.fault(r, areaReturning, 12):
			names = append(names, q.columns[r.IntN(len(q.columns))])
		default:
			names = append(names, q.spell(r, shape[r.IntN(len(shape))].Name.Value))
		}
	}
	return true, names
}

// Decide makes every subquery that the sequence writes from now on one whose ORDER BY decides its
// first row: one with an ORDER BY ends it with the primary key of its table, and one without has an
// ORDER BY of that key alone, so that rows that tie under the rest come in the order of the key. A
// test that cannot see which of several first rows the engine took, as a test through database/sql
// cannot, calls it before the first call; every other draw of the sequence stays as it is.
func (q *Sequence) Decide() {
	q.decided = true
}

// subqueriesOf the select, the update, or the delete s, in the order of the text, or nil.
func subqueriesOf(s stmt.Statement) []stmt.Select {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries
	case *stmt.Update:
		return s.Subqueries
	case *stmt.Delete:
		return s.Subqueries
	}
	return nil
}

// subqueries gives the conditions cs of the WHERE of a select, an update, or a delete of table with
// the scalar subqueries of slice 15 among them, for one statement in [Profile.Subquery], and cs
// otherwise. One time in six there are two. Each is the value of a comparison: one time in three that
// of a comparison of cs, in place of its value, and otherwise of a new condition at the end of cs, on
// the key one time in two, as goqite's receive is, and else on any column of the shape, with = one
// time in two and else any comparison. The value is on the left one time in three. Its select is the
// one that [Sequence.innerSelect] draws for the column. One time in ten the WHERE then has one more
// condition that is never true, a comparison with NULL or a test for NULL of the key, so that the
// statement computes no subquery. Of an update, one in [Profile.Receive] of those with subqueries is
// goqite's receive, as [Sequence.receive] draws it, in place of cs. All draws are from the stream of
// the area, which a profile without [Profile.Subquery] has off, and nothing is drawn there.
func (q *Sequence) subqueries(table stmt.Name, cs []stmt.Condition, update bool) []stmt.Condition {
	p := q.profile
	if !p.on(areaSubquery) {
		return cs
	}
	r := q.rSubquery
	if r.IntN(p.Subquery) != 0 {
		return cs
	}
	if update && p.Receive > 0 && r.IntN(p.Receive) == 0 {
		return q.receive(r, table)
	}
	shape := q.shapeOf(table)
	key := &shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	cs = slices.Clone(cs)
	n := 1
	if r.IntN(6) == 0 {
		n = 2
	}
	for range n {
		var compared []int
		for i, c := range cs {
			if slices.Contains(comparisons, c.Operator) && c.Value.Subquery == 0 {
				compared = append(compared, i)
			}
		}
		var c *stmt.Condition
		if len(compared) > 0 && r.IntN(3) == 0 {
			c = &cs[compared[r.IntN(len(compared))]]
		} else {
			column := key
			if r.IntN(2) == 0 {
				column = &shape[r.IntN(len(shape))]
			}
			op := stmt.OpEqual
			if r.IntN(2) == 0 {
				op = comparisons[r.IntN(len(comparisons))]
			}
			cs = append(cs, stmt.Condition{Column: stmt.Name{Raw: q.spell(r, column.Name.Value)}, Operator: op})
			c = &cs[len(cs)-1]
		}
		c.ValueFirst = r.IntN(3) == 0
		var column *stmt.Column
		if c.Column.Raw != "" {
			column = columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
		}
		q.inner = append(q.inner, q.innerSelect(r, table, column))
		c.Value = stmt.Term{Subquery: len(q.inner)}
	}
	if r.IntN(10) == 0 {
		never := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: stmt.OpIsNull}
		if r.IntN(2) == 0 {
			column := shape[r.IntN(len(shape))]
			never = stmt.Condition{Column: stmt.Name{Raw: q.spell(r, column.Name.Value)}, Operator: comparisons[r.IntN(len(comparisons))], Value: stmt.Term{Value: stmt.Null{}}}
		}
		cs = slices.Insert(cs, r.IntN(len(cs)+1), never)
	}
	return cs
}

// innerSelect of a subquery that is compared with the column of the shape of table, or nil for a
// name that the shape does not have, with the random numbers of r:
//
//   - Its table is, one time in two, another table of the sequence that it created and did not drop,
//     if there is one; one time in twelve of the rest one that it dropped or never created, a fault
//     of the area; and else that of the statement, in any spelling. A table that has no column of the
//     type of the column that the subquery is compared with gives way to the table of the statement,
//     which has the column itself, but one time in twelve, a fault of the area.
//   - Its column is * one time in eight, and one time in 32 for a table of more than one column, which
//     s4ql refuses. Otherwise, one time in eight, a name that its table does not have and the table of
//     the statement has, a correlated name, which s4ql refuses; one time in twelve any column of its
//     table; and else a column of its table of the type of the column that it is compared with, INT
//     and INTEGER being one, if it has one, and one time in two one that takes NULL if one does. Each
//     of the two faults is a quarter as likely in a profile without the subqueries among
//     [Profile.Faults], as [Sequence.fault] says.
//   - Its WHERE is none, and its ORDER BY its column ascending, one time in three for a column that
//     takes NULL, where NULL comes first.
//   - Otherwise its WHERE is, one time in four of a table of which the sequence keeps an index, the one that
//     [Sequence.aimedWhere] draws, with its ORDER BY, which is goqite's receive where two indexes
//     begin alike, with LIMIT 1, or the one that [Sequence.indexConditions] draws, each one time in
//     two; two times in eight none; one time in eight the key equal to a value that an insert gave
//     it, which finds one row or none; and else as [Sequence.conditions] draws one, with the faults
//     of a select, so that it mostly reads its table.
//   - Otherwise its ORDER BY, but for an aimed one, is none two times in eight; the key, ascending or
//     descending, which decides the first row, one time in eight; its own column two times in eight,
//     ascending two times in three, where NULL comes first; the columns that [Sequence.orderColumns]
//     draws for its WHERE, as for a select, one time in eight; or a column of the table, alone, where
//     rows tie, or with the key after it, which decides. With [Sequence.Decide], it ends with the key.
//   - Its LIMIT is 0 one time in eight, from 2 to 4 one time in sixteen, none three times in sixteen
//     but for an aimed one, and else 1.
func (q *Sequence) innerSelect(r Rand, table stmt.Name, column *stmt.Column) stmt.Select {
	var same, live, dead []string
	for _, raw := range q.tables {
		fold := nameOf(raw, 0).Fold()
		switch {
		case fold == table.Fold():
			same = append(same, raw)
		case q.live[fold]:
			live = append(live, raw)
		default:
			dead = append(dead, raw)
		}
	}
	if len(same) == 0 {
		same = []string{table.Raw}
	}
	var sub stmt.Select
	switch {
	case len(live) > 0 && r.IntN(2) == 0:
		sub.Table.Raw = live[r.IntN(len(live))]
	case len(dead) > 0 && q.fault(r, areaSubquery, 12):
		sub.Table.Raw = dead[r.IntN(len(dead))]
	default:
		sub.Table.Raw = same[r.IntN(len(same))]
	}
	inner := nameOf(sub.Table.Raw, 0)
	shape := q.shapeOf(inner)
	alikeOf := func(shape []stmt.Column) []string {
		var alike []string
		for _, c := range shape {
			if column != nil && kindOf(c.Type) == kindOf(column.Type) {
				alike = append(alike, c.Name.Value)
			}
		}
		return alike
	}
	alike := alikeOf(shape)
	if column != nil && len(alike) == 0 && !q.fault(r, areaSubquery, 12) {
		// The table of the statement, which has the column itself.
		sub.Table.Raw = same[r.IntN(len(same))]
		inner = nameOf(sub.Table.Raw, 0)
		shape = q.shapeOf(inner)
		alike = alikeOf(shape)
	}
	key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	var correlated []string
	for _, c := range q.shapeOf(table) {
		if columnOfShape(shape, c.Name.Value) == nil {
			correlated = append(correlated, c.Name.Value)
		}
	}
	// result is the fold of the column of the subquery in its table, or "".
	result := ""
	switch {
	case r.IntN(8) == 0 && (len(shape) == 1 || r.IntN(4) == 0):
	case len(correlated) > 0 && q.fault(r, areaSubquery, 8):
		sub.Columns = []stmt.Name{{Raw: q.spell(r, correlated[r.IntN(len(correlated))])}}
	case q.fault(r, areaSubquery, 12) || len(alike) == 0:
		result = shape[r.IntN(len(shape))].Name.Value
	default:
		result = alike[r.IntN(len(alike))]
		// One time in two a column that takes NULL, if one is alike, where NULL can come first.
		var nullable []string
		for _, fold := range alike {
			if c := columnOfShape(shape, fold); !c.PrimaryKey && !c.NotNull {
				nullable = append(nullable, fold)
			}
		}
		if len(nullable) > 0 && r.IntN(2) == 0 {
			result = nullable[r.IntN(len(nullable))]
		}
	}
	switch {
	case result != "":
		sub.Columns = []stmt.Name{{Raw: q.spell(r, result)}}
	case sub.Columns == nil:
		result = shape[0].Name.Value
	default:
		// An ORDER BY of the column of a correlated name orders by the key.
		result = key.Name.Value
	}
	// The WHERE, with the state of the select of the statement kept for its own ORDER BY.
	selected, aimed, aimedLimit := q.selected, q.aimed, q.aimedLimit
	defer func() { q.selected, q.aimed, q.aimedLimit = selected, aimed, aimedLimit }()
	q.aimed = nil
	stored := q.stored[[2]string{inner.Fold(), key.Name.Value}]
	// nullFirst is set for a subquery of a column that takes NULL, one time in three, which reads every
	// row of its table in the order of its column, where NULL comes first.
	nullFirst := false
	if c := columnOfShape(shape, result); sub.Columns != nil && c != nil && !c.PrimaryKey && !c.NotNull {
		nullFirst = r.IntN(3) == 0
	}
	switch n := r.IntN(8); {
	case nullFirst:
	case n < 2 && len(q.indexed[inner.Fold()]) > 0 && r.IntN(2) == 0:
		sub.Where = q.aimedWhere(r, inner)
	case n < 2 && len(q.indexed[inner.Fold()]) > 0:
		sub.Where = q.indexConditions(r, inner)
	case n < 4:
	case n == 4 && len(stored) > 0:
		sub.Where = []stmt.Condition{{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: stmt.OpEqual, Value: stmt.Term{Value: stored[r.IntN(len(stored))]}}}
	default:
		// With the faults of a select, so that a WHERE of the subquery mostly reads its table.
		sub.Where = q.conditions(r, areaSelects, inner)
	}
	var by []orderColumn
	var named []stmt.OrderColumn
	switch n := r.IntN(8); {
	case nullFirst:
		by = []orderColumn{{fold: result}}
	case q.aimed != nil:
		by = q.aimed
	case n < 2:
	case n == 2:
		by = []orderColumn{{key.Name.Value, r.IntN(2) == 0}}
	case n == 3, n == 7:
		by = []orderColumn{{result, r.IntN(3) == 0}}
	case n == 4:
		q.selected = sub.Where
		named, _ = q.orderColumns(r, inner)
	default:
		c := shape[r.IntN(len(shape))]
		by = []orderColumn{{c.Name.Value, r.IntN(2) == 0}}
		if !c.PrimaryKey && n == 5 {
			by = append(by, orderColumn{key.Name.Value, r.IntN(2) == 0})
		}
	}
	for _, c := range by {
		named = append(named, stmt.OrderColumn{Name: stmt.Name{Raw: q.spell(r, c.fold)}, Descending: c.descending})
	}
	if q.decided && !slices.ContainsFunc(named, func(c stmt.OrderColumn) bool { return nameOf(c.Name.Raw, 0).Fold() == key.Name.Value }) {
		named = append(named, stmt.OrderColumn{Name: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Descending: r.IntN(2) == 0})
	}
	// A column of ORDER BY once: SQLite takes a later one, and the generator writes no name twice.
	seen := map[string]bool{}
	for _, c := range named {
		if fold := nameOf(c.Name.Raw, 0).Fold(); !seen[fold] {
			seen[fold] = true
			sub.OrderBy = append(sub.OrderBy, c)
		}
	}
	switch n := r.IntN(16); {
	case n < 2:
		sub.Limit.Value = stmt.Integer(0)
	case n == 2:
		sub.Limit.Value = stmt.Integer(2 + r.IntN(3))
	case n < 5 && q.aimed == nil:
	default:
		sub.Limit.Value = stmt.Integer(1)
	}
	return sub
}

// receive gives the WHERE of goqite's receive of a message on table, with the random numbers of r: one
// time in four the key alone, and otherwise a pin of a column outside the key first, by = with a value
// that an insert gave the column three times in four, if there is one, and otherwise as
// [Sequence.conditionValue] draws it, as goqite's queue = ?; and then the key equal to a subquery of
// table of its key, whose WHERE is the one that [Sequence.aimedWhere] draws for an index of the
// table, if the sequence keeps one, with its ORDER BY, where the pin of the statement is then its
// first pin, and otherwise the same pin and a bound or two of another column, each open or closed, of
// a value drawn as for the pin, as goqite's ? >= timeout and received < ?, with an ORDER BY
// of two columns outside the key, or of one and the key, each ascending or descending, as goqite's
// priority desc, created; and LIMIT 1. With [Sequence.Decide], the ORDER BY ends with the key.
func (q *Sequence) receive(r Rand, table stmt.Name) []stmt.Condition {
	shape := q.shapeOf(table)
	key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	var others []stmt.Column
	for _, c := range shape {
		if !c.PrimaryKey {
			others = append(others, c)
		}
	}
	// value of a condition on the column c: three times in four one that an insert gave it, if there is
	// one, so that the receive finds a message.
	value := func(c stmt.Column) stmt.Value {
		if stored := q.stored[[2]string{table.Fold(), c.Name.Value}]; len(stored) > 0 && r.IntN(4) != 0 {
			return stored[r.IntN(len(stored))]
		}
		return q.conditionValue(r, areaSelects, table, c.Name.Value, c.Type)
	}
	var cs []stmt.Condition
	if len(others) > 0 && r.IntN(4) != 0 {
		c := others[r.IntN(len(others))]
		cs = append(cs, stmt.Condition{Column: stmt.Name{Raw: q.spell(r, c.Name.Value)}, Operator: stmt.OpEqual, Value: stmt.Term{Value: value(c)}})
	}
	sub := stmt.Select{Table: stmt.Name{Raw: table.Raw}, Columns: []stmt.Name{{Raw: q.spell(r, key.Name.Value)}}, Limit: stmt.Term{Value: stmt.Integer(1)}}
	aimed, aimedLimit := q.aimed, q.aimedLimit
	defer func() { q.aimed, q.aimedLimit = aimed, aimedLimit }()
	q.aimed = nil
	var by []orderColumn
	if len(q.indexed[table.Fold()]) > 0 {
		sub.Where = q.aimedWhere(r, table)
		by = q.aimed
		// The pin of the statement is the pin of its subquery, as goqite's queue = ? is in both.
		if pin := slices.IndexFunc(sub.Where, func(c stmt.Condition) bool { return c.Operator == stmt.OpEqual }); cs != nil && pin >= 0 {
			cs = []stmt.Condition{sub.Where[pin]}
		}
	}
	if by == nil {
		sub.Where = slices.Clone(cs)
		if len(others) > 1 {
			c := others[r.IntN(len(others))]
			for range 1 + r.IntN(2) {
				sub.Where = append(sub.Where, stmt.Condition{Column: stmt.Name{Raw: q.spell(r, c.Name.Value)}, Operator: bounds[r.IntN(len(bounds))], Value: stmt.Term{Value: value(c)}})
			}
		}
		order := slices.Clone(others)
		shuffle(r, order)
		if len(order) < 2 || r.IntN(4) == 0 {
			order = append(order[:min(1, len(order))], key)
		}
		for _, c := range order[:min(2, len(order))] {
			by = append(by, orderColumn{c.Name.Value, r.IntN(2) == 0})
		}
	}
	if q.profile.Ties > 0 && r.IntN(4) != 0 {
		// A queue whose messages are ready, as goqite's mostly are: each pin is of one of the two values
		// of [Profile.Ties], which many rows share, and each bound holds both, so that the receive finds
		// several messages; and an ORDER BY of two columns outside the key, the pinned one first, if
		// there is a pin, on which every message that the WHERE finds ties, as on goqite's priority
		// where messages share one, so that the messages tie where they share the value of the second,
		// as on goqite's created where messages are sent in the same millisecond.
		for i, c := range sub.Where {
			column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
			switch {
			case column == nil || column.PrimaryKey:
			case c.Operator == stmt.OpEqual:
				sub.Where[i].Value = stmt.Term{Value: tieValue(column.Type, r.IntN(2))}
			case c.Operator == stmt.OpGreater || c.Operator == stmt.OpGreaterOrEqual:
				sub.Where[i].Operator, sub.Where[i].Value = stmt.OpGreaterOrEqual, stmt.Term{Value: tieValue(column.Type, 0)}
			case c.Operator == stmt.OpLess || c.Operator == stmt.OpLessOrEqual:
				sub.Where[i].Operator, sub.Where[i].Value = stmt.OpLessOrEqual, stmt.Term{Value: tieValue(column.Type, 1)}
			}
		}
		// The pin of the statement stays the pin of its subquery.
		for _, w := range sub.Where {
			if len(cs) > 0 && w.Operator == stmt.OpEqual && nameOf(w.Column.Raw, 0).Fold() == nameOf(cs[0].Column.Raw, 0).Fold() {
				cs[0] = w
				break
			}
		}
		var first []orderColumn
		if len(cs) > 0 {
			first = []orderColumn{{nameOf(cs[0].Column.Raw, 0).Fold(), r.IntN(2) == 0}}
		}
		// The second is the next of the aimed ORDER BY one time in two, which an index delivers, and
		// otherwise another column, which the engine may sort, and then draws among the messages that
		// tie.
		var rest []orderColumn
		if r.IntN(2) == 0 {
			rest = slices.DeleteFunc(slices.Clone(by), func(c orderColumn) bool {
				return c.fold == key.Name.Value || len(first) > 0 && c.fold == first[0].fold
			})
		}
		order := slices.Clone(others)
		shuffle(r, order)
		for _, c := range order {
			if !slices.ContainsFunc(append(first, rest...), func(o orderColumn) bool { return o.fold == c.Name.Value }) {
				rest = append(rest, orderColumn{c.Name.Value, r.IntN(2) == 0})
			}
		}
		if by = append(first, rest...); len(by) > 2 {
			by = by[:2]
		}
	}
	if q.decided && !slices.ContainsFunc(by, func(c orderColumn) bool { return c.fold == key.Name.Value }) {
		by = append(by, orderColumn{key.Name.Value, r.IntN(2) == 0})
	}
	for _, c := range by {
		sub.OrderBy = append(sub.OrderBy, stmt.OrderColumn{Name: stmt.Name{Raw: q.spell(r, c.fold)}, Descending: c.descending})
	}
	q.inner = append(q.inner, sub)
	q.received = true
	return append(cs, stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: stmt.OpEqual, Value: stmt.Term{Subquery: len(q.inner)}})
}

// orderColumn is a column of an ORDER BY that a [Sequence] draws: the fold of its name, and whether
// it is descending.
type orderColumn struct {
	fold       string
	descending bool
}

// orderColumns of the ORDER BY of a select of table, with the random numbers of r, and whether they
// are aimed at an index beside one of a range. Two times in three for a table of which the sequence
// keeps an index, as [Sequence.indexed] says, they are the columns of the key of one of them, the
// index's columns and then those of the primary key that it lacks: where the WHERE of the select
// pins the leading columns of the keys of two indexes, as many of each, and bounds the next column
// of one of them, three times in four the columns after the pins of the other, so that with a LIMIT
// the index that delivers the order wins over the one of the range, as goqite's receive reads by
// goqite's index beside one on the queue and the timeout; and otherwise its leading columns. Of
// these, from the first to all are taken, of which a column that the WHERE pins, with = or IS NULL,
// is left out one time in two. They are in the directions of the key, or all turned one time in two.
// But for those aimed at the index beside one of a range, one time in five they are the first column
// of a key of three columns or more and the columns after a gap of one or more, if the sequence
// keeps such an index; and three times in five two leading columns of the key or more, with no
// column left out, of which the first or the second is turned again, so that the directions are
// mixed. Otherwise, one time in six, the primary key, ascending or
// descending; and else one column of the shape one time in two, and from one to all of them, each
// once, in any order, else, each descending one time in two. Then, one time in four, a column that
// the WHERE pins is put at any place, if the WHERE pins one that the columns lack. One time in
// twelve, a name that the shape does not have is put at any place, a fault, a quarter as likely in a
// profile without the order among [Profile.Faults], as [Sequence.fault] says.
func (q *Sequence) orderColumns(r Rand, table stmt.Name) ([]stmt.OrderColumn, bool) {
	shape := q.shapeOf(table)
	var pinned, bounded []string
	for _, c := range q.selected {
		fold := nameOf(c.Column.Raw, 0).Fold()
		switch {
		case columnOfShape(shape, fold) == nil:
		case c.Operator == stmt.OpEqual || c.Operator == stmt.OpIsNull:
			pinned = append(pinned, fold)
		case slices.Contains(bounds, c.Operator):
			bounded = append(bounded, fold)
		}
	}
	var columns []orderColumn
	aimed := false
	indexed := q.indexed[table.Fold()]
	switch {
	case len(indexed) > 0 && r.IntN(3) != 0:
		keys := q.keysOf(table)
		// The primary key is a candidate of the range too.
		for _, c := range shape {
			if c.PrimaryKey {
				keys = append(keys, []orderColumn{{fold: c.Name.Value}})
			}
		}
		// lead is the number of the leading columns of the key that the WHERE pins.
		lead := func(key []orderColumn) int {
			n := 0
			for n < len(key) && slices.Contains(pinned, key[n].fold) {
				n++
			}
			return n
		}
		var delivering [][]orderColumn
		for i, key := range keys {
			for j, other := range keys {
				if n := lead(key); i != j && j < len(indexed) && n == lead(other) && n < len(key) && n < len(other) && slices.Contains(bounded, key[n].fold) && !slices.Contains(bounded, other[n].fold) {
					delivering = append(delivering, other[n:])
				}
			}
		}
		key := keys[r.IntN(len(indexed))]
		if len(delivering) > 0 && r.IntN(4) != 0 {
			key, aimed = delivering[r.IntN(len(delivering))], true
		}

		skip := r.IntN(2) == 0
		turned := r.IntN(2) == 0
		taken := key[:1+r.IntN(len(key))]
		var long [][]orderColumn
		for _, k := range keys[:len(indexed)] {
			if len(k) > 2 {
				long = append(long, k)
			}
		}
		// The shape of the ORDER BY: 0 for a gap, 1 to 3 for mixed directions, and 4 for neither.
		form := 4
		if !aimed {
			form = r.IntN(5)
		}
		switch {
		case form == 0 && len(long) > 0:
			// The first column of a key of three columns or more, and the columns after a gap of one or
			// more.
			key = long[r.IntN(len(long))]
			at := 1 + r.IntN(len(key)-2)
			taken = append([]orderColumn{key[0]}, key[at+1:at+2+r.IntN(len(key)-at-1)]...)
		case form == 0:
			form = 4
		case form < 4 && len(key) > 1:
			// Two leading columns of the key at least, and no column left out, so that the directions
			// are mixed among columns that the key could deliver.
			taken, skip = key[:2+r.IntN(len(key)-1)], false
		}
		for _, k := range taken {
			if skip && slices.Contains(pinned, k.fold) {
				continue
			}
			columns = append(columns, orderColumn{k.fold, k.descending != turned})
		}
		if len(columns) > 1 && form > 0 && form < 4 {
			// The first or the second column, so that the turn shows where the rows tie in the first.
			c := &columns[r.IntN(2)]
			c.descending = !c.descending
		}
	case r.IntN(6) == 0:
		key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
		columns = []orderColumn{{key.Name.Value, r.IntN(2) == 0}}
	default:
		order := make([]int, len(shape))
		for i := range order {
			order[i] = i
		}
		shuffle(r, order)
		n := 1
		if r.IntN(2) == 0 {
			n += r.IntN(len(order))
		}
		for _, position := range order[:n] {
			columns = append(columns, orderColumn{shape[position].Name.Value, r.IntN(2) == 0})
		}
	}
	var lacked []string
	for _, fold := range pinned {
		if !slices.ContainsFunc(columns, func(c orderColumn) bool { return c.fold == fold }) && !slices.Contains(lacked, fold) {
			lacked = append(lacked, fold)
		}
	}
	if len(lacked) > 0 && r.IntN(4) == 0 {
		columns = slices.Insert(columns, r.IntN(len(columns)+1), orderColumn{lacked[r.IntN(len(lacked))], r.IntN(2) == 0})
	}
	var named []stmt.OrderColumn
	for _, c := range columns {
		named = append(named, stmt.OrderColumn{Name: stmt.Name{Raw: q.spell(r, c.fold)}, Descending: c.descending})
	}
	if q.fault(r, areaOrder, 12) {
		raw := q.missing(r, shape)
		named = slices.Insert(named, r.IntN(len(named)+1), stmt.OrderColumn{Name: stmt.Name{Raw: raw}, Descending: r.IntN(2) == 0})
	}
	return named, aimed
}

// keysOf the indexes that the sequence keeps of table, as [Sequence.indexed] says, in the order in
// which it keeps them: for each, the columns of the index and then those of the primary key that it
// lacks, as the key of the index is.
func (q *Sequence) keysOf(table stmt.Name) [][]orderColumn {
	shape := q.shapeOf(table)
	var keys [][]orderColumn
	for _, ix := range q.indexed[table.Fold()] {
		var key []orderColumn
		for _, c := range ix.columns {
			key = append(key, orderColumn{c.Name.Value, c.Descending})
		}
		for _, c := range shape {
			if c.PrimaryKey && !slices.ContainsFunc(key, func(k orderColumn) bool { return k.fold == c.Name.Value }) {
				key = append(key, orderColumn{fold: c.Name.Value})
			}
		}
		keys = append(keys, key)
	}
	return keys
}

// aimedWhere gives the conditions of the WHERE of a select of table, of which the sequence keeps an
// index, with the random numbers of r, and sets the ORDER BY that they are drawn for,
// [Sequence.aimed], as [Profile.Aimed] asks. Where two of the indexes begin with the same column,
// which is not the primary key, and differ in the next, one time in two it is goqite's receive: that
// first column pinned, the next column of one of the two bounded once or twice, each bound open or
// closed, and an ORDER BY of the columns of the key of the other after its first, from one to all,
// with a LIMIT, so that the index that delivers the order wins over the one that narrows a range.
// Otherwise one column of the key of an index, not its last and not the primary key, is pinned, at
// the start or in the middle of the key, and the ORDER BY is of the columns of the key around it, up
// to the one after it at least, which the key delivers with the pinned column taken out of it; one
// time in three the pinned column stays in the ORDER BY at its place, and the select has a LIMIT one
// time in two. A pin at the start has, one time in two, a bound or two on the next column too, each
// open or closed, which the scan bounds while it delivers the order. A pin is by =, with a value as [Sequence.conditionValue] draws it, or by IS NULL one
// time in four for a column that takes NULL. The columns of the ORDER BY are in the directions of
// the key, or all turned one time in two. The conditions are in any order one time in three. Where
// no key has two columns, a pin of which leaves another, the select has no WHERE and no aim.
func (q *Sequence) aimedWhere(r Rand, table stmt.Name) []stmt.Condition {
	shape := q.shapeOf(table)
	keys := q.keysOf(table)
	inKey := func(fold string) bool {
		c := columnOfShape(shape, fold)
		return c != nil && c.PrimaryKey
	}
	typeOf := func(fold string) stmt.Type {
		if c := columnOfShape(shape, fold); c != nil {
			return c.Type
		}
		return anyType(r)
	}
	pin := func(fold string) stmt.Condition {
		c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, fold)}, Operator: stmt.OpEqual}
		if column := columnOfShape(shape, fold); column != nil && !column.PrimaryKey && !column.NotNull && r.IntN(4) == 0 {
			c.Operator = stmt.OpIsNull
		} else {
			c.Value.Value = q.conditionValue(r, areaIndexes, table, fold, typeOf(fold))
		}
		return c
	}
	turn := func(key []orderColumn) []orderColumn {
		turned := r.IntN(2) == 0
		var by []orderColumn
		for _, k := range key {
			by = append(by, orderColumn{k.fold, k.descending != turned})
		}
		return by
	}
	var pairs [][2]int
	for i, a := range keys {
		for j, b := range keys {
			if i != j && len(a) > 1 && len(b) > 1 && a[0].fold == b[0].fold && a[1].fold != b[1].fold && !inKey(a[0].fold) {
				pairs = append(pairs, [2]int{i, j})
			}
		}
	}
	var cs []stmt.Condition
	if len(pairs) > 0 && r.IntN(2) == 0 {
		pair := pairs[r.IntN(len(pairs))]
		delivering, ranged := keys[pair[0]], keys[pair[1]]
		cs = append(cs, pin(delivering[0].fold))
		for range 1 + r.IntN(2) {
			c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, ranged[1].fold)}, Operator: bounds[r.IntN(len(bounds))]}
			c.Value.Value = q.conditionValue(r, areaIndexes, table, ranged[1].fold, typeOf(ranged[1].fold))
			cs = append(cs, c)
		}
		rest := delivering[1:]
		q.aimed, q.aimedLimit = turn(rest[:1+r.IntN(len(rest))]), true
	} else {
		type place struct{ key, at int }
		var places []place
		for i, key := range keys {
			for at := range len(key) - 1 {
				if !inKey(key[at].fold) {
					places = append(places, place{i, at})
				}
			}
		}
		if len(places) == 0 {
			return nil
		}
		p := places[r.IntN(len(places))]
		key := keys[p.key]
		cs = append(cs, pin(key[p.at].fold))
		if p.at == 0 && r.IntN(2) == 0 {
			// A range of the column after the pin, which the scan of the key bounds while it delivers the
			// order.
			for range 1 + r.IntN(2) {
				c := stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key[1].fold)}, Operator: bounds[r.IntN(len(bounds))]}
				c.Value.Value = q.conditionValue(r, areaIndexes, table, key[1].fold, typeOf(key[1].fold))
				cs = append(cs, c)
			}
		}
		keep := r.IntN(3) == 0
		var by []orderColumn
		for i, k := range key {
			if i != p.at || keep {
				by = append(by, k)
			}
		}
		least := p.at + 1
		if keep {
			least++
		}
		q.aimed, q.aimedLimit = turn(by[:least+r.IntN(len(by)-least+1)]), r.IntN(2) == 0
	}
	if r.IntN(3) == 0 {
		shuffle(r, cs)
	}
	return cs
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
	if indexed := q.indexed[table.Fold()]; q.profile.Shared > 0 && len(indexed) > 0 && len(shape) > 1 && r.IntN(q.profile.Shared) == 0 {
		// The first column of an index that the sequence keeps, and then another column, so that the
		// two indexes pin as many columns where a WHERE pins the first, and one can deliver an order
		// where the other narrows a range, as goqite's indexes do.
		first := indexed[r.IntN(len(indexed))].columns[0]
		var others []int
		for i, c := range shape {
			if c.Name.Value != first.Name.Value {
				others = append(others, i)
			}
		}
		next := shape[others[r.IntN(len(others))]]
		descending := r.IntN(3) == 0
		columns = []stmt.IndexColumn{{Name: stmt.Name{Raw: q.spell(r, first.Name.Value)}, Descending: first.Descending}, {Name: stmt.Name{Raw: q.spell(r, next.Name.Value)}, Descending: descending}}
		kept = []stmt.IndexColumn{first, {Name: stmt.Name{Value: next.Name.Value}, Descending: descending}}
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

// indexConditions of a WHERE of a statement of table, with the random numbers of r, which an index of
// the sequence on table can serve, or nil if the sequence keeps no index of table, as [Sequence.indexed] says: for the columns
// of one of them, the leading ones, from one to all, or with [Profile.Indexes] of 2 all of them one
// time in two, each pinned by = with a value as [Sequence.conditionValue] draws it, or by IS NULL one
// time in three for a column that takes NULL and one time in eight otherwise. The key of the index is
// its columns, and the key of the table after them if it is not among them. With [Profile.Indexes] of
// 2, one time in four, the next column of that key after the pins, if it has one, is pinned by = too;
// otherwise, one time in two, that column has a bound or two, each open or closed. One time in eight
// the WHERE has no pin, and a bound or two on the first column of the index. Then, one time in three,
// it has one more condition, one of those that [Sequence.conditions] draws. The conditions are in any
// order one time in three.
func (q *Sequence) indexConditions(r Rand, table stmt.Name) []stmt.Condition {
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
// values of the assignments are kept, as [Sequence.store] keeps those of an insert. An update of
// [Profile.ByKey], of [Profile.NewKey], or of [Profile.EveryRow] has the SET and the WHERE that the
// field says.
func (q *Sequence) update(table stmt.Name) ([]stmt.Assignment, []stmt.Condition) {
	r := q.rChanges
	shape := q.shapeOf(table)
	key := slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })
	stored := q.stored[[2]string{table.Fold(), shape[key].Name.Value}]
	byKey := q.profile.ByKey > 0 && len(shape) > 1 && len(stored) > 0 && r.IntN(q.profile.ByKey) == 0
	newKey := !byKey && q.profile.NewKey > 0 && len(stored) > 0 && r.IntN(q.profile.NewKey) == 0
	everyRow := !byKey && !newKey && q.profile.EveryRow > 0 && len(shape) > 1 && r.IntN(q.profile.EveryRow) == 0
	order := make([]int, len(shape))
	for i := range order {
		order[i] = i
	}
	for i := len(order) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	switch {
	case byKey, everyRow:
		// The columns outside the key, as many as for another update.
		order = slices.DeleteFunc(order, func(position int) bool { return position == key })
	case newKey:
		// The key first, and then as many columns as for another update.
		order = append([]int{key}, slices.DeleteFunc(order, func(position int) bool { return position == key })...)
	}
	order = order[:1+r.IntN(len(order))]
	if len(order) > 1 && !byKey && !newKey && !everyRow && r.IntN(2) == 0 {
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
		case newKey && i == 0 && i != missing:
			// A key that no insert gives, so that the row moves to a key that it did not have.
			v = keyOfStep(c.Type, q.step)
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
	if byKey || newKey {
		return set, q.subqueries(table, []stmt.Condition{{Column: stmt.Name{Raw: q.spell(r, shape[key].Name.Value)}, Operator: stmt.OpEqual, Value: stmt.Term{Value: stored[r.IntN(len(stored))]}}}, true)
	}
	if everyRow || r.IntN(4) == 0 {
		return set, q.subqueries(table, nil, true)
	}
	return set, q.subqueries(table, q.changeConditions(table), true)
}

// delete gives the conditions of a delete from table, as [Generator.Delete] gives them: none one
// time in five, which deletes every row, and otherwise a WHERE as [Sequence.changeConditions] draws
// one.
func (q *Sequence) delete(table stmt.Name) []stmt.Condition {
	if q.rChanges.IntN(5) == 0 {
		return q.subqueries(table, nil, false)
	}
	return q.subqueries(table, q.changeConditions(table), false)
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
		return q.indexConditions(q.rIndexes, table)
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
	return q.like(q.rArguments, a)
}

// like a, an argument, as [Sequence.Like] says, with the random numbers of r.
func (q *Sequence) like(r Rand, a any) any {
	if s, ok := a.(string); ok && (!utf8.ValidString(s) || strings.ContainsRune(s, 0)) {
		return a
	}
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
	// s4ql supports. It is false for a text of several statements.
	Supported bool
	// Statements is the number of statements of a text of several statements that [Sequence.several]
	// wrote, from 2, and 0 for every other text.
	Statements int
	// Each reports, of a text of several statements, whether each of them is a statement of the slices
	// that a Sequence writes, which s4ql supports, as Supported reports it of a text of one.
	Each bool
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
	if odds := q.profile.Around[map[bool]int{true: 0, false: 1}[len(c.Args) > 0]]; (c.Supported || c.Each) && odds > 0 && q.rArguments.IntN(odds) == 0 {
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

// returns reports whether s is an insert, an update, or a delete with RETURNING.
func returns(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Insert:
		return s.Returning.Present()
	case *stmt.Update:
		return s.Returning.Present()
	case *stmt.Delete:
		return s.Returning.Present()
	}
	return false
}

// next call of the sequence, with an argument for each parameter of a statement of the
// slices that a sequence writes, and for each ? of a text that is not.
func (q *Sequence) next() Call {
	p := q.profile
	if p.on(areaSubquery) && p.Again > 0 && q.again != nil && q.rSubquery.IntN(p.Again) == 0 {
		// The text of an earlier statement with a subquery again, as a prepared statement runs, with
		// each argument the same one time in two and otherwise another of its Go type, and for a LIMIT
		// an integer from 0 to 4, which s4ql takes.
		r := q.rSubquery
		c := *q.again
		c.Args = slices.Clone(c.Args)
		for i, a := range c.Args {
			if r.IntN(2) != 0 {
				continue
			}
			if slices.Contains(q.limits, i+1) {
				c.Args[i] = int64(r.IntN(5))
				continue
			}
			c.Args[i] = q.like(r, a)
		}
		return c
	}
	if p.on(areaArithmetic) && q.rArithmetic.IntN(8*p.Arithmetic) == 0 {
		// A text that the statement pass refuses for its arithmetic.
		r := q.rArithmetic
		form := ArithmeticRefused[r.IntN(len(ArithmeticRefused))]
		return Call{Src: fmt.Sprintf(form, q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))]), Query: r.IntN(2) == 0}
	}
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
	if p.on(areaSeveral) && q.rSeveral.IntN(p.Several) == 0 {
		return q.several()
	}
	return q.statement()
}

// statement of the sequence: the next call of a statement that the generator writes, of the kind
// that the weights of the profile draw, with an argument for each parameter.
func (q *Sequence) statement() Call {
	p := q.profile
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
	q.inner = nil
	src, s, args := q.g.CallOf(k)
	if p.on(areaSubquery) {
		// An argument of the LIMIT of a subquery that s4ql refuses, as for that of a select below.
		r := q.rSubquery
		for _, sub := range subqueriesOf(s) {
			if sub.Limit.Parameter > 0 && q.fault(r, areaSubquery, 6) {
				args[sub.Limit.Parameter-1] = []any{int64(-1 - r.IntN(3)), 2.0, 1.5, "1", "x", []byte("1"), nil}[r.IntN(7)]
			}
		}
	}
	if sel, ok := s.(*stmt.Select); ok && sel.Limit.Parameter > 0 && q.fault(q.rOrder, areaOrder, 2) {
		// An argument of the LIMIT that s4ql refuses: a negative integer, a real, text, a blob, or NULL,
		// of which SQLite takes some as a number and refuses others.
		r := q.rOrder
		args[sel.Limit.Parameter-1] = []any{int64(-1 - r.IntN(3)), int64(math.MinInt64), 2.0, 1.5, "2", "x", []byte("2"), nil}[r.IntN(8)]
	}
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
	// A query, and an insert, an update, or a delete with RETURNING, of slice 14, runs through Exec
	// one time in eight, and every other statement through Query one time in eight.
	c := Call{Src: src, Args: args, Query: (k == KindSelect || returns(s)) != (r.IntN(8) == 0), Supported: true}
	if subqueriesOf(s) != nil {
		q.again, q.limits = &c, nil
		if sel, ok := s.(*stmt.Select); ok && sel.Limit.Parameter > 0 {
			q.limits = append(q.limits, sel.Limit.Parameter)
		}
		for _, sub := range subqueriesOf(s) {
			if sub.Limit.Parameter > 0 {
				q.limits = append(q.limits, sub.Limit.Parameter)
			}
		}
	}
	return c
}

// The classes of the values of arithmetic, by the rule of exact types: INT and INTEGER hold one.
const (
	classInteger = "integer"
	classReal    = "real"
	classText    = "text"
	classBlob    = "blob"
)

// classOf the values of the column c of a shape, or of the value v where c is nil, for a column that
// the shape does not have: an integer for NULL and every other value.
func classOf(c *stmt.Column, v stmt.Value) string {
	t := stmt.TypeInt
	switch {
	case c != nil:
		t = c.Type
	case v != nil:
		switch v.(type) {
		case stmt.Real:
			t = stmt.TypeReal
		case stmt.Text:
			t = stmt.TypeText
		case stmt.Blob:
			t = stmt.TypeBlob
		}
	}
	switch kindOf(t) {
	case stmt.TypeReal:
		return classReal
	case stmt.TypeText:
		return classText
	case stmt.TypeBlob:
		return classBlob
	}
	return classInteger
}

// columnNode, valueNode, and operatorNode are the nodes of the trees of arithmetic that the hooks of a
// sequence give the generator.
func columnNode(raw string) stmt.Expression {
	return stmt.Expression{Kind: stmt.ExprColumn, Column: stmt.Name{Raw: raw}}
}

func valueNode(v stmt.Value) stmt.Expression {
	return stmt.Expression{Kind: stmt.ExprValue, Value: v}
}

func operatorNode(k stmt.ExpressionKind, operands ...stmt.Expression) stmt.Expression {
	return stmt.Expression{Kind: k, Operands: operands}
}

// arithmeticHooks wrap the hooks of the WHERE of a select, of an update, and of a delete, so that their
// conditions and assignments, and the WHERE of their subqueries, have arithmetic, as
// [Sequence.arithmeticWhere] and [Sequence.arithmeticSet] draw it. The assignments are drawn first,
// then the conditions of the statement, and then those of each subquery in the order of the text.
func (q *Sequence) arithmeticHooks() {
	where, update, del := q.g.Where, q.g.Update, q.g.Delete
	q.g.Where = func(table stmt.Name) []stmt.Condition {
		q.failingRow = false
		cs := where(table)
		switch r := q.rArithmetic; {
		case r.IntN(2*q.profile.Arithmetic) == 0:
			cs = q.failingWhere(table, cs)
		case r.IntN(3*q.profile.Arithmetic) == 0:
			cs = q.keyWhere(table, cs)
		}
		cs = q.arithmeticWhere(table, cs)
		q.arithmeticInner()
		return cs
	}
	q.g.Update = func(table stmt.Name) ([]stmt.Assignment, []stmt.Condition) {
		set, cs := update(table)
		set, cs = q.arithmeticSet(table, set), q.arithmeticWhere(table, cs)
		q.arithmeticInner()
		return set, cs
	}
	q.g.Delete = func(table stmt.Name) []stmt.Condition {
		cs := q.arithmeticWhere(table, del(table))
		q.arithmeticInner()
		return cs
	}
}

// failingWhere of a select of table, in place of the conditions cs, for one select in
// 2*[Profile.Arithmetic]: a WHERE of one comparison of a column x of the shape of a class of numbers,
// the key one time in two, with a number divided by x minus a value that an insert gave x, as
// `x != 10 / (x - 3)`, which holds for most rows and fails for the row of the value, or for a real
// one time in two x times 1e308, which fails for the rows above 1.8 in size; or cs where the
// sequence keeps no such value. A LIMIT, as [Sequence.order] gives one then, makes the rows that the
// select reads, and the direction that the engine draws, decide whether it fails.
func (q *Sequence) failingWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r := q.rArithmetic
	shape := q.shapeOf(table)
	var numbers []stmt.Column
	for _, c := range shape {
		if class := classOf(&c, nil); class == classInteger || class == classReal {
			numbers = append(numbers, c)
		}
	}
	if len(numbers) == 0 {
		return cs
	}
	x := numbers[r.IntN(len(numbers))]
	for _, c := range numbers {
		if c.PrimaryKey && r.IntN(2) == 0 {
			x = c
		}
	}
	class := classOf(&x, nil)
	var stored []stmt.Value
	for _, v := range q.stored[[2]string{table.Fold(), x.Name.Value}] {
		if classOf(nil, v) == class && v != stmt.Value(stmt.Null{}) {
			stored = append(stored, v)
		}
	}
	if len(stored) == 0 {
		return cs
	}
	ten := stmt.Value(stmt.Integer(10))
	if class == classReal {
		ten = stmt.Real(10)
	}
	e := operatorNode(stmt.ExprDivide, valueNode(ten), operatorNode(stmt.ExprSubtract, columnNode(q.spell(r, x.Name.Value)), valueNode(stored[r.IntN(len(stored))])))
	if class == classReal && r.IntN(2) == 0 {
		// Infinite where x is above 1.8 in size.
		e = operatorNode(stmt.ExprMultiply, columnNode(q.spell(r, x.Name.Value)), valueNode(stmt.Real(1e308)))
	}
	op := []stmt.Operator{stmt.OpNotEqual, stmt.OpNotEqual, stmt.OpLess, stmt.OpGreater}[r.IntN(4)]
	q.failingRow = true
	return []stmt.Condition{{Column: stmt.Name{Raw: q.spell(r, x.Name.Value)}, Operator: op, Value: stmt.Term{Expression: e}}}
}

// keyWhere of a select of table, in place of the conditions cs, for one select in
// 3*[Profile.Arithmetic] of those that [Sequence.failingWhere] leaves: the key equal to an expression
// without a column whose value is one that an insert gave the key, as [Sequence.constantOf] draws it,
// which gets the row of the key, or one time in three the key between two such expressions; or cs
// where the sequence keeps no such value, or the key is of no class of numbers.
func (q *Sequence) keyWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r := q.rArithmetic
	shape := q.shapeOf(table)
	key := shape[slices.IndexFunc(shape, func(c stmt.Column) bool { return c.PrimaryKey })]
	class := classOf(&key, nil)
	var stored []stmt.Value
	for _, v := range q.stored[[2]string{table.Fold(), key.Name.Value}] {
		if classOf(nil, v) == class && v != stmt.Value(stmt.Null{}) {
			stored = append(stored, v)
		}
	}
	if len(stored) == 0 || class != classInteger && class != classReal {
		return cs
	}
	at := func(op stmt.Operator) stmt.Condition {
		return stmt.Condition{Column: stmt.Name{Raw: q.spell(r, key.Name.Value)}, Operator: op, Value: stmt.Term{Expression: q.constantOf(r, stored[r.IntN(len(stored))], class)}}
	}
	if r.IntN(3) == 0 {
		return []stmt.Condition{at([]stmt.Operator{stmt.OpGreater, stmt.OpGreaterOrEqual}[r.IntN(2)]), at([]stmt.Operator{stmt.OpLess, stmt.OpLessOrEqual}[r.IntN(2)])}
	}
	return []stmt.Condition{at(stmt.OpEqual)}
}

// arithmeticInner gives the WHERE of each subquery that the hooks of the statement gave arithmetic, as
// [Sequence.arithmeticWhere] draws it for the table of the subquery.
func (q *Sequence) arithmeticInner() {
	for i := range q.inner {
		q.inner[i].Where = q.arithmeticWhere(nameOf(q.inner[i].Table.Raw, 0), q.inner[i].Where)
	}
}

// arithmeticWhere gives the conditions cs of a WHERE of table, of a select, an update, a delete, or a
// subquery, with one comparison in [Profile.Arithmetic] that is not a test for NULL and has no
// subquery as its value made one of arithmetic, as [Sequence.arithmeticCondition] draws it. One
// comparison in half of [Profile.Arithmetic] whose value is a subquery tests in place of its column
// the expression of [Sequence.testedOf], of another class one time in two, which the subquery is then
// compared with; of a TEXT or a BLOB column only as a fault of the area, since s4ql refuses
// arithmetic there. The others are as they were.
func (q *Sequence) arithmeticWhere(table stmt.Name, cs []stmt.Condition) []stmt.Condition {
	r, p := q.rArithmetic, q.profile
	if len(cs) == 0 {
		return cs
	}
	shape := q.shapeOf(table)
	cs = slices.Clone(cs)
	for i, c := range cs {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || c.Expression.Present() || c.Value.Expression.Present() {
			continue
		}
		if c.Value.Subquery > 0 {
			if r.IntN(max(1, p.Arithmetic/2)) != 0 {
				continue
			}
			column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
			class := classOf(column, nil)
			if (class == classText || class == classBlob) && !q.fault(r, areaArithmetic, 6) {
				continue
			}
			cs[i].Column, cs[i].Expression = stmt.Name{}, q.testedOf(r, shape, c.Column.Raw, class, 2)
			continue
		}
		if r.IntN(p.Arithmetic) == 0 {
			cs[i] = q.arithmeticCondition(r, shape, c)
		}
	}
	return cs
}

// arithmeticCondition gives the comparison c, of a column of the shape or of a name that the shape
// does not have, with arithmetic, drawn from r. A comparison of a TEXT or a BLOB column stays as it
// is, but as a fault of the area, since s4ql refuses arithmetic there, and then one time in two its
// value is an expression of the class of NULL, as [Sequence.nullExpression] draws it, which s4ql
// refuses too. Otherwise, of the class of the column, as [classOf] gives it, in 14 draws:
//
//   - three times, the value is an expression without a column whose value is that of c, as
//     [Sequence.constantOf] draws it, so that the condition serves the key or an index as c would;
//   - two times, the comparison tests the column in an expression, `c + d`, `c - d`, `1 * c`, or
//     `-c`, against a value that keeps it true of the same rows where nothing overflows, which
//     serves nothing;
//   - once, the value is another column, as [Sequence.otherColumn] gives it;
//   - once, the value is an expression of another column and a number;
//   - three times, the value is one that fails for every row, for some, or for none, as
//     [Sequence.failing] draws it;
//   - once, both sides are a column times a number;
//   - two times, the comparison tests the column in an expression of [Sequence.testedOf] against a
//     plain value, a literal or a ?, as [Sequence.plainOf] draws it;
//   - and once, as a fault of the area one time in two, the value of c plus a number of the other
//     class of numbers, and else a value as for the first case.
func (q *Sequence) arithmeticCondition(r Rand, shape []stmt.Column, c stmt.Condition) stmt.Condition {
	column := columnOfShape(shape, nameOf(c.Column.Raw, 0).Fold())
	class := classOf(column, c.Value.Value)
	if class == classText || class == classBlob {
		if !q.fault(r, areaArithmetic, 6) {
			return c
		}
		if r.IntN(2) == 0 {
			c.Value = stmt.Term{Expression: q.nullExpression(r, shape)}
			return c
		}
	}
	v := c.Value.Value
	raw := c.Column.Raw
	switch n := r.IntN(14); {
	case n < 3:
		c.Value = stmt.Term{Expression: q.constantOf(r, v, class)}
	case n < 5:
		d := q.small(r, class)
		var tested, value stmt.Expression
		switch r.IntN(4) {
		case 0:
			tested, value = operatorNode(stmt.ExprAdd, columnNode(raw), valueNode(d)), operatorNode(stmt.ExprAdd, valueNode(v), valueNode(d))
		case 1:
			tested, value = operatorNode(stmt.ExprSubtract, columnNode(raw), valueNode(d)), operatorNode(stmt.ExprSubtract, valueNode(v), valueNode(d))
		case 2:
			tested, value = operatorNode(stmt.ExprMultiply, valueNode(q.one(class)), columnNode(raw)), q.constantOf(r, v, class)
		default:
			tested, value = operatorNode(stmt.ExprNegate, columnNode(raw)), operatorNode(stmt.ExprNegate, valueNode(v))
			c.Operator = map[stmt.Operator]stmt.Operator{stmt.OpLess: stmt.OpGreater, stmt.OpGreater: stmt.OpLess,
				stmt.OpLessOrEqual: stmt.OpGreaterOrEqual, stmt.OpGreaterOrEqual: stmt.OpLessOrEqual, stmt.OpEqual: stmt.OpEqual, stmt.OpNotEqual: stmt.OpNotEqual}[c.Operator]
		}
		c.Column, c.Expression, c.Value = stmt.Name{}, tested, stmt.Term{Expression: value}
	case n < 6:
		c.Value, c.ValueFirst = stmt.Term{Expression: columnNode(q.otherColumn(r, shape, class))}, false
	case n < 7:
		k := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply}[r.IntN(3)]
		c.Value = stmt.Term{Expression: q.nested(r, operatorNode(k, columnNode(q.otherColumn(r, shape, class)), valueNode(q.small(r, class))), class)}
	case n < 10:
		c.Value = stmt.Term{Expression: q.failing(r, shape, class, raw)}
		q.failingRow = q.failingRow || c.Value.Expression.HasColumn()
	case n < 11:
		c.Column, c.Expression = stmt.Name{}, operatorNode(stmt.ExprMultiply, columnNode(raw), valueNode(q.small(r, class)))
		c.Value, c.ValueFirst = stmt.Term{Expression: operatorNode(stmt.ExprMultiply, columnNode(q.otherColumn(r, shape, class)), valueNode(q.small(r, class)))}, false
	case n < 13:
		c.Column, c.Expression = stmt.Name{}, q.testedOf(r, shape, raw, class, 3)
		c.Value = stmt.Term{Value: q.plainOf(r, v, class)}
	default:
		if q.fault(r, areaArithmetic, 2) {
			c.Value = stmt.Term{Expression: operatorNode(stmt.ExprAdd, valueNode(v), valueNode(q.small(r, otherClass(class))))}
		} else {
			c.Value = stmt.Term{Expression: q.constantOf(r, v, class)}
		}
	}
	return c
}

// otherClass of numbers than class: a real for an integer, and an integer for every other.
func otherClass(class string) string {
	if class == classInteger {
		return classReal
	}
	return classInteger
}

// testedOf the column of the shape whose raw text is raw, of the class, as the side that a comparison
// tests against a plain value or a subquery: an expression that keeps the value of the column, `raw +
// 0`, `raw - 0`, `1 * raw`, or `raw / 1`, with numbers of the class; one time in six `raw + ?` with
// NULL, of the class of NULL, which meets every class of numbers; and, as a fault of the area one
// time in wrong, an expression of another class of numbers, `raw + 0.0` for an integer, and for a
// real an integer column of the shape times 1, where the shape has one.
func (q *Sequence) testedOf(r Rand, shape []stmt.Column, raw, class string, wrong int) stmt.Expression {
	zero := stmt.Value(stmt.Integer(0))
	if class == classReal {
		zero = stmt.Real(0)
	}
	if q.fault(r, areaArithmetic, wrong) {
		if class != classReal {
			return operatorNode(stmt.ExprAdd, columnNode(raw), valueNode(stmt.Real(0)))
		}
		var integers []stmt.Column
		for _, c := range shape {
			if classOf(&c, nil) == classInteger {
				integers = append(integers, c)
			}
		}
		if len(integers) > 0 {
			return operatorNode(stmt.ExprMultiply, columnNode(q.spell(r, integers[r.IntN(len(integers))].Name.Value)), valueNode(stmt.Integer(1)))
		}
	}
	switch r.IntN(6) {
	case 0:
		return operatorNode(stmt.ExprAdd, columnNode(raw), valueNode(stmt.Null{}))
	case 1:
		return operatorNode(stmt.ExprSubtract, columnNode(raw), valueNode(zero))
	case 2:
		return operatorNode(stmt.ExprMultiply, valueNode(q.one(class)), columnNode(raw))
	case 3:
		return operatorNode(stmt.ExprDivide, columnNode(raw), valueNode(q.one(class)))
	}
	return operatorNode(stmt.ExprAdd, columnNode(raw), valueNode(zero))
}

// plainOf the value v of a comparison of a column of the class, for a side that it tests in an
// expression: v, or, as a fault of the area one time in three, a value of another class, a number of
// the other class of numbers one time in two, and else a text or a blob, which s4ql refuses against
// an expression.
func (q *Sequence) plainOf(r Rand, v stmt.Value, class string) stmt.Value {
	if !q.fault(r, areaArithmetic, 3) {
		return v
	}
	switch r.IntN(4) {
	case 0:
		return stmt.Text("a")
	case 1:
		return stmt.Blob{0x01}
	}
	return q.small(r, otherClass(class))
}

// nullExpression of the class of NULL, from r, for a comparison or an assignment of a column of the
// shape: NULL, which the generator writes as a ?, and a number, or one time in two a column of the
// shape of a class of numbers, where it has one, minus NULL.
func (q *Sequence) nullExpression(r Rand, shape []stmt.Column) stmt.Expression {
	var numbers []stmt.Column
	for _, c := range shape {
		if class := classOf(&c, nil); class == classInteger || class == classReal {
			numbers = append(numbers, c)
		}
	}
	if len(numbers) > 0 && r.IntN(2) == 0 {
		return operatorNode(stmt.ExprSubtract, columnNode(q.spell(r, numbers[r.IntN(len(numbers))].Name.Value)), valueNode(stmt.Null{}))
	}
	k := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply}[r.IntN(3)]
	return operatorNode(k, valueNode(stmt.Null{}), valueNode(q.small(r, classInteger)))
}

// integerExpression of the class integer, from r, for an assignment of a column of the shape: an
// integer column of the shape, where it has one, plus a number one time in two, and else an
// expression without a column whose value is a small number, as [Sequence.constantOf] draws it.
func (q *Sequence) integerExpression(r Rand, shape []stmt.Column) stmt.Expression {
	var integers []stmt.Column
	for _, c := range shape {
		if classOf(&c, nil) == classInteger {
			integers = append(integers, c)
		}
	}
	if len(integers) > 0 && r.IntN(2) == 0 {
		return operatorNode(stmt.ExprAdd, columnNode(q.spell(r, integers[r.IntN(len(integers))].Name.Value)), valueNode(q.small(r, classInteger)))
	}
	return q.constantOf(r, q.small(r, classInteger), classInteger)
}

// arithmeticSet gives the assignments set of an update of table with one in [Profile.Arithmetic]
// given arithmetic, drawn from the stream of the area. An assignment of a TEXT or a BLOB column stays
// as it is, but as a fault of the area, and then one time in two its value is an expression of the
// class of NULL, as [Sequence.nullExpression] draws it. One of a REAL column is, as a fault of the
// area one time in eight, an expression of the class integer, as [Sequence.integerExpression] draws
// it. s4ql refuses both. Otherwise, of the class of the column, as [classOf] gives it:
// three times in ten the column that it sets and a number, as `n = n + 1`, also of the key, which the
// update then computes; one time in ten another column, as [Sequence.otherColumn] gives it, and a
// number, of the other class as a fault of the area one time in two; two times in ten another column
// alone, of its class or, as a fault of the area one time in two, of any; one time in ten an
// expression without a column equal to its value, as [Sequence.constantOf] draws it; two times in ten
// a value that fails for every row, for some, or for none, as [Sequence.failing] draws it; and one
// time in ten an expression of the column and another, three operators deep. One assignment after the first in 2*[Profile.Arithmetic],
// before the draw of the others, is a column of its class that the SET sets before and a number,
// which reads the value of the row before the statement; and one SET in 2*[Profile.Arithmetic] gets
// one more such assignment at its end, of the first column of a class of numbers that it does not
// set, from the first column of the class that it sets.
func (q *Sequence) arithmeticSet(table stmt.Name, set []stmt.Assignment) []stmt.Assignment {
	r, p := q.rArithmetic, q.profile
	shape := q.shapeOf(table)
	set = slices.Clone(set)
	for i, a := range set {
		if a.Value.Expression.Present() {
			continue
		}
		column := columnOfShape(shape, nameOf(a.Column.Raw, 0).Fold())
		class := classOf(column, a.Value.Value)
		if i > 0 && r.IntN(2*p.Arithmetic) == 0 {
			// A column of its class that the SET sets before, and a number, whose value before the
			// statement the assignment reads.
			j := slices.IndexFunc(set[:i], func(b stmt.Assignment) bool {
				return classOf(columnOfShape(shape, nameOf(b.Column.Raw, 0).Fold()), b.Value.Value) == class
			})
			if j >= 0 && (class == classInteger || class == classReal) {
				k := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply}[r.IntN(3)]
				set[i].Value = stmt.Term{Expression: operatorNode(k, columnNode(set[j].Column.Raw), valueNode(q.small(r, class)))}
				continue
			}
		}
		if r.IntN(p.Arithmetic) != 0 {
			continue
		}
		switch {
		case class == classText || class == classBlob:
			if !q.fault(r, areaArithmetic, 6) {
				continue
			}
			if r.IntN(2) == 0 {
				set[i].Value = stmt.Term{Expression: q.nullExpression(r, shape)}
				continue
			}
		case class == classReal && column != nil && q.fault(r, areaArithmetic, 8):
			set[i].Value = stmt.Term{Expression: q.integerExpression(r, shape)}
			continue
		}
		self := a.Column.Raw
		if column != nil {
			self = q.spell(r, column.Name.Value)
		}
		k := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply, stmt.ExprDivide}[r.IntN(4)]
		var e stmt.Expression
		switch n := r.IntN(10); {
		case n < 3:
			e = q.nested(r, operatorNode(k, columnNode(self), valueNode(q.small(r, class))), class)
		case n < 4:
			other := class
			if q.fault(r, areaArithmetic, 2) {
				other = otherClass(class)
			}
			e = operatorNode(k, columnNode(q.otherColumn(r, shape, class)), valueNode(q.small(r, other)))
		case n < 6:
			// A column alone, of the class or, as a fault of the area one time in two, of any class, which
			// s4ql refuses where it is of another class; another than the one that it sets where the shape
			// has one.
			of := shape
			if !q.fault(r, areaArithmetic, 2) {
				of = slices.DeleteFunc(slices.Clone(shape), func(c stmt.Column) bool { return classOf(&c, nil) != class })
			}
			if others := slices.DeleteFunc(slices.Clone(of), func(c stmt.Column) bool { return column != nil && c.Name.Value == column.Name.Value }); len(others) > 0 {
				of = others
			}
			if len(of) == 0 {
				of = shape
			}
			e = columnNode(q.spell(r, of[r.IntN(len(of))].Name.Value))
		case n < 7:
			e = q.constantOf(r, a.Value.Value, class)
		case n < 9:
			e = q.failing(r, shape, class, self)
		default:
			sum := operatorNode(stmt.ExprAdd, columnNode(self), columnNode(q.otherColumn(r, shape, class)))
			e = operatorNode(stmt.ExprSubtract, operatorNode(stmt.ExprMultiply, sum, valueNode(q.small(r, class))), operatorNode(stmt.ExprDivide, columnNode(self), valueNode(q.small(r, class))))
		}
		set[i].Value = stmt.Term{Expression: e}
	}
	if r.IntN(2*p.Arithmetic) == 0 {
		// One more assignment, of a column that the SET does not set, from a column of its class that it
		// sets before, which reads the value of the row before the statement.
		for _, c := range shape {
			class := classOf(&c, nil)
			if class != classInteger && class != classReal || slices.ContainsFunc(set, func(a stmt.Assignment) bool { return nameOf(a.Column.Raw, 0).Fold() == c.Name.Value }) {
				continue
			}
			j := slices.IndexFunc(set, func(a stmt.Assignment) bool {
				return classOf(columnOfShape(shape, nameOf(a.Column.Raw, 0).Fold()), nil) == class && columnOfShape(shape, nameOf(a.Column.Raw, 0).Fold()) != nil
			})
			if j < 0 {
				continue
			}
			k := []stmt.ExpressionKind{stmt.ExprAdd, stmt.ExprSubtract, stmt.ExprMultiply}[r.IntN(3)]
			value := operatorNode(k, columnNode(set[j].Column.Raw), valueNode(q.small(r, class)))
			return append(set, stmt.Assignment{Column: stmt.Name{Raw: q.spell(r, c.Name.Value)}, Value: stmt.Term{Expression: value}})
		}
	}
	return set
}

// small number of the class at random from r: 1, 2, 3, or -1 for an integer, and 0.5, 2.0, -1.5, or
// 1.0 for a real, as for every other class.
func (q *Sequence) small(r Rand, class string) stmt.Value {
	if class == classReal {
		return stmt.Real([]float64{0.5, 2, -1.5, 1}[r.IntN(4)])
	}
	return stmt.Integer([]int64{1, 2, 3, -1}[r.IntN(4)])
}

// one of the class: 1, or 1.0 for a real.
func (q *Sequence) one(class string) stmt.Value {
	if class == classReal {
		return stmt.Real(1)
	}
	return stmt.Integer(1)
}

// nested e, of the class, one time in four in another operator that keeps its value but where it
// overflows: times 1, divided by 1, or, if e is no negation, negated and then times -1.
func (q *Sequence) nested(r Rand, e stmt.Expression, class string) stmt.Expression {
	if r.IntN(4) != 0 {
		return e
	}
	minus := stmt.Value(stmt.Integer(-1))
	if class == classReal {
		minus = stmt.Real(-1)
	}
	switch n := r.IntN(3); {
	case n == 0:
		return operatorNode(stmt.ExprMultiply, e, valueNode(q.one(class)))
	case n == 1 || e.Kind == stmt.ExprNegate:
		return operatorNode(stmt.ExprDivide, e, valueNode(q.one(class)))
	}
	return operatorNode(stmt.ExprMultiply, operatorNode(stmt.ExprNegate, e), valueNode(minus))
}

// constantOf the value v, for a column of the class: an expression without a column whose value is v,
// where v is an integer or a real: v and a number added and taken away, or the other way, v times
// one, v divided by one, or the negation of -v, as [Sequence.nested] may nest it; and for NULL, text,
// or a blob, which the generator writes as a ?, v plus a number of the class, which is NULL for NULL
// and refused for the others.
func (q *Sequence) constantOf(r Rand, v stmt.Value, class string) stmt.Expression {
	var e stmt.Expression
	switch v := v.(type) {
	case stmt.Integer:
		d := stmt.Integer(1 + r.IntN(3))
		switch n := r.IntN(5); {
		case n == 0 && v > math.MinInt64+3:
			e = operatorNode(stmt.ExprAdd, valueNode(v-d), valueNode(d))
		case n == 1 && v < math.MaxInt64-3:
			e = operatorNode(stmt.ExprSubtract, valueNode(v+d), valueNode(d))
		case n == 2 && v != math.MinInt64:
			e = operatorNode(stmt.ExprNegate, valueNode(-v))
		case n == 3:
			e = operatorNode(stmt.ExprDivide, valueNode(v), valueNode(stmt.Integer(1)))
		default:
			e = operatorNode(stmt.ExprMultiply, valueNode(v), valueNode(stmt.Integer(1)))
		}
	case stmt.Real:
		switch r.IntN(4) {
		case 0:
			e = operatorNode(stmt.ExprAdd, valueNode(v), valueNode(stmt.Real(0)))
		case 1:
			e = operatorNode(stmt.ExprNegate, valueNode(-v))
		case 2:
			e = operatorNode(stmt.ExprDivide, valueNode(v), valueNode(stmt.Real(1)))
		default:
			e = operatorNode(stmt.ExprMultiply, valueNode(stmt.Real(1)), valueNode(v))
		}
	default:
		return operatorNode(stmt.ExprAdd, valueNode(v), valueNode(q.small(r, class)))
	}
	return q.nested(r, e, class)
}

// otherColumn of the shape at random of the class, the raw text of a spelling of its name: one time
// in six, as a fault of the area, any column of the shape, and otherwise one of the class, or any if
// none is.
func (q *Sequence) otherColumn(r Rand, shape []stmt.Column, class string) string {
	var of []stmt.Column
	for _, c := range shape {
		if classOf(&c, nil) == class {
			of = append(of, c)
		}
	}
	if len(of) == 0 || q.fault(r, areaArithmetic, 6) {
		of = shape
	}
	return q.spell(r, of[r.IntN(len(of))].Name.Value)
}

// failing value of the class, for a comparison or an assignment whose column has the raw text raw: one
// time in three an expression without a column that fails whatever the rows are, of an integer
// overflow, a division by zero, or a real result that is not finite; and otherwise an expression of
// raw, or one time in two of another column, as [Sequence.otherColumn] gives it, that fails for the
// rows whose value is large, or for the row of a value that an insert gave the column, and not for
// the others.
func (q *Sequence) failing(r Rand, shape []stmt.Column, class, raw string) stmt.Expression {
	if r.IntN(2) == 0 {
		raw = q.otherColumn(r, shape, class)
	}
	x := columnNode(raw)
	fold := nameOf(raw, 0).Fold()
	var stored []stmt.Value
	for _, vs := range q.storedOf(fold) {
		if classOf(nil, vs) == class && vs != stmt.Value(stmt.Null{}) {
			stored = append(stored, vs)
		}
	}
	at := func(zero stmt.Value) stmt.Value {
		if len(stored) == 0 {
			return zero
		}
		return stored[r.IntN(len(stored))]
	}
	if class == classReal {
		switch r.IntN(9) {
		case 0:
			return operatorNode(stmt.ExprMultiply, valueNode(stmt.Real(1e308)), valueNode(stmt.Real(10)))
		case 1:
			return operatorNode(stmt.ExprSubtract, valueNode(stmt.Real(-math.MaxFloat64)), valueNode(stmt.Real(1e308)))
		case 2:
			return operatorNode(stmt.ExprDivide, valueNode(stmt.Real(2.5)), operatorNode(stmt.ExprSubtract, valueNode(stmt.Real(0.5)), valueNode(stmt.Real(0.5))))
		case 3:
			return operatorNode(stmt.ExprDivide, valueNode(stmt.Real(1)), valueNode(stmt.Real(0)))
		case 4, 5:
			return operatorNode(stmt.ExprMultiply, x, valueNode(stmt.Real(1e308)))
		case 6:
			return operatorNode(stmt.ExprDivide, x, valueNode(stmt.Real(1e-300)))
		}
		return operatorNode(stmt.ExprDivide, valueNode(stmt.Real(1.5)), operatorNode(stmt.ExprSubtract, x, valueNode(at(stmt.Real(0)))))
	}
	switch r.IntN(10) {
	case 0:
		return operatorNode(stmt.ExprAdd, valueNode(stmt.Integer(math.MaxInt64)), valueNode(stmt.Integer(1+r.IntN(3))))
	case 1:
		return operatorNode(stmt.ExprNegate, valueNode(stmt.Integer(math.MinInt64)))
	case 2:
		return operatorNode(stmt.ExprDivide, valueNode(stmt.Integer(7)), operatorNode(stmt.ExprSubtract, valueNode(stmt.Integer(2)), valueNode(stmt.Integer(2))))
	case 3:
		return operatorNode(stmt.ExprDivide, valueNode(stmt.Integer(math.MinInt64)), valueNode(stmt.Integer(-1)))
	case 4:
		return operatorNode(stmt.ExprAdd, x, valueNode(stmt.Integer(math.MaxInt64)))
	case 5:
		return operatorNode(stmt.ExprSubtract, x, valueNode(stmt.Integer(math.MaxInt64)))
	case 6:
		return operatorNode(stmt.ExprMultiply, x, valueNode(stmt.Integer(4611686018427387904)))
	case 7:
		return operatorNode(stmt.ExprNegate, x)
	}
	return operatorNode(stmt.ExprDivide, valueNode(stmt.Integer(10)), operatorNode(stmt.ExprSubtract, x, valueNode(at(stmt.Integer(0)))))
}

// storedOf the column of the fold: the values that the sequence keeps for it, of every table, in the
// order of the names of the tables, as [Sequence.store] kept them.
func (q *Sequence) storedOf(fold string) []stmt.Value {
	var keys [][2]string
	for k := range q.stored {
		if k[1] == fold {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	var vs []stmt.Value
	for _, k := range keys {
		vs = append(vs, q.stored[k]...)
	}
	return vs
}

// ArithmeticRefused are texts that the statement pass refuses for their arithmetic, with %[1]v for the
// name of a table and %[2]v for that of a column, each the refusal of the text that its comment names:
// a string, a blob, and NULL as operands, a division by a literal zero and by a real that SQLite reads
// as zero, an integer literal outside 64 bits, a second sign, a + sign, ~, a binary operator that is
// not arithmetic, a function, parentheses around a single value, more than 100 open at once,
// arithmetic in a select list, in an ORDER BY, and before IS, and two sides without a column.
var ArithmeticRefused = []string{
	"select * from %[1]v where %[2]v = 'a' + 1",                      // string in arithmetic
	"update %[1]v set %[2]v = x'01' * %[2]v",                         // blob in arithmetic
	"delete from %[1]v where %[2]v < null - 1",                       // NULL in arithmetic
	"select * from %[1]v where %[2]v = 1 / 0",                        // division by zero
	"update %[1]v set %[2]v = %[2]v / -0.0",                          // division by zero
	"select * from %[1]v where %[2]v > %[2]v / 1e-400",               // division by zero
	"update %[1]v set %[2]v = %[2]v - 9223372036854775808",           // integer outside 64 bits
	"select * from %[1]v where %[2]v = - -%[2]v",                     // second sign
	"update %[1]v set %[2]v = +%[2]v",                                // + sign
	"select * from %[1]v where %[2]v = ~1 + %[2]v",                   // ~ operator
	"delete from %[1]v where %[2]v = %[2]v %% 2",                     // binary operator
	"update %[1]v set %[2]v = abs(%[2]v) + 1",                        // function call
	"select * from %[1]v where %[2]v = (1) + %[2]v",                  // parentheses
	"select * from %[1]v where %[2]v = " + deepParentheses + "%[2]v", // parentheses deeper than 100
	"select %[2]v + 1 from %[1]v",                                    // result column other than a column or *
	"select * from %[1]v order by %[2]v * 2",                         // ordering term other than a column
	"select * from %[1]v where %[2]v + 1 is null",                    // binary operator before IS
	"select * from %[1]v where 1 + 1 = 2 * 2",                        // value compared with a value
}

// deepParentheses opens 101 parentheses of arithmetic, each around 1 + and the next, and closes them.
var deepParentheses = strings.Repeat("(1 + ", 101) + "1" + strings.Repeat(")", 101) + " + "
