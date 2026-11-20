package core_test

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store/simstore"
)

// The model is a second, simpler account of what the database holds: tables and rows in maps,
// changed by its own code, and never by the code under test. The tests compare everything that the
// core returns and everything that it stores with it.

// mtable is a table of the model.
type mtable struct {
	create *s4qlpb.CreateTable
	// rows by the text of their primary key.
	rows map[string][]*s4qlpb.Value
	// indexes of the table, in the order in which they were created.
	indexes []*s4qlpb.CreateIndex
}

// key of a row of the table: the values of its primary-key columns, in key order.
func (t *mtable) key(row []*s4qlpb.Value) []*s4qlpb.Value {
	key := make([]*s4qlpb.Value, len(t.create.GetPrimaryKey()))
	for i, position := range t.create.GetPrimaryKey() {
		key[i] = row[position]
	}
	return key
}

// sorted rows of the table, in the order of the primary key.
func (t *mtable) sorted() [][]*s4qlpb.Value {
	rows := make([][]*s4qlpb.Value, 0, len(t.rows))
	for _, row := range t.rows {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int { return compareKeys(t.key(a), t.key(b)) })
	return rows
}

// mkeyColumn is a column of the key of an index or of a primary key in the model.
type mkeyColumn struct {
	position   uint32
	descending bool
}

// keyOf the index ci of the table, or of its primary key if ci is nil: the columns of the index,
// each in its direction, and then the primary-key columns that the index does not hold, ascending.
func (t *mtable) keyOf(ci *s4qlpb.CreateIndex) []mkeyColumn {
	var key []mkeyColumn
	for _, c := range ci.GetColumns() {
		key = append(key, mkeyColumn{c.GetColumn(), c.GetDescending()})
	}
	for _, position := range t.create.GetPrimaryKey() {
		if !slices.ContainsFunc(key, func(c mkeyColumn) bool { return c.position == position }) {
			key = append(key, mkeyColumn{position: position})
		}
	}
	return key
}

// sortedBy gives the rows of the table in the order of the key of its index ci, or of its primary
// key if ci is nil, or in the reverse of it, with NULL before every other value of a column: see
// [mtable.keyOf].
func (t *mtable) sortedBy(ci *s4qlpb.CreateIndex, backward bool) [][]*s4qlpb.Value {
	key := t.keyOf(ci)
	rows := make([][]*s4qlpb.Value, 0, len(t.rows))
	for _, row := range t.rows {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int {
		for _, c := range key {
			d := compareValues(a[c.position], b[c.position])
			if c.descending != backward {
				d = -d
			}
			if d != 0 {
				return d
			}
		}
		return 0
	})
	return rows
}

// scanned rows of the table by its index ci, or by its primary key if ci is nil, as the scan says,
// in the order of [mtable.sortedBy]: those whose values in the leading columns of the key are
// those of Equal, where a NULL is the same as a NULL only, and whose value in the column after
// those is within both bounds, which no NULL is.
func (t *mtable) scanned(ci *s4qlpb.CreateIndex, scan core.IndexScan) [][]*s4qlpb.Value {
	key := t.keyOf(ci)
	// within reports whether v is within the bound b, which is the lower bound if lower is true.
	within := func(v *s4qlpb.Value, b core.Bound, lower bool) bool {
		switch {
		case b.Value == nil:
			return true
		case v.WhichKind() == s4qlpb.Value_Null_case:
			return false
		}
		d := compareValues(v, b.Value)
		if !lower {
			d = -d
		}
		return d > 0 || d == 0 && !b.Open
	}
	var rows [][]*s4qlpb.Value
	for _, row := range t.sortedBy(ci, scan.Backward) {
		matches := true
		for i, v := range scan.Equal {
			matches = matches && compareValues(row[key[i].position], v) == 0
		}
		if len(scan.Equal) < len(key) {
			v := row[key[len(scan.Equal)].position]
			matches = matches && within(v, scan.Lower, true) && within(v, scan.Upper, false)
		}
		if matches {
			rows = append(rows, row)
		}
	}
	return rows
}

// compareValues of one column as SQLite orders them: NULL before every other value, and the others
// as [compareKeys] compares them.
func compareValues(a, b *s4qlpb.Value) int {
	switch an, bn := a.WhichKind() == s4qlpb.Value_Null_case, b.WhichKind() == s4qlpb.Value_Null_case; {
	case an && bn:
		return 0
	case an:
		return -1
	case bn:
		return 1
	}
	return compareKeys([]*s4qlpb.Value{a}, []*s4qlpb.Value{b})
}

// compareKeys as SQLite orders the keys of a table, whose columns each hold values of one kind:
// integers and reals by their number, with -0.0 the same as 0.0, and text and blobs byte by byte, as
// the collation BINARY compares text.
func compareKeys(a, b []*s4qlpb.Value) int {
	for i := range a {
		var c int
		switch a[i].WhichKind() {
		case s4qlpb.Value_Integer_case:
			c = cmp.Compare(a[i].GetInteger(), b[i].GetInteger())
		case s4qlpb.Value_Real_case:
			c = cmp.Compare(a[i].GetReal(), b[i].GetReal())
		case s4qlpb.Value_Text_case:
			c = strings.Compare(a[i].GetText(), b[i].GetText())
		case s4qlpb.Value_Blob_case:
			c = bytes.Compare(a[i].GetBlob(), b[i].GetBlob())
		default:
			panic(fmt.Sprint("a key of kind ", a[i].WhichKind()))
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// stored values as a column of SQLite holds them: a real that is -0.0 as 0.0. The values that are
// not changed are shared.
func stored(values []*s4qlpb.Value) []*s4qlpb.Value {
	out := slices.Clone(values)
	for i, v := range out {
		if v.WhichKind() == s4qlpb.Value_Real_case && v.GetReal() == 0 {
			out[i] = s4qlpb.Value_builder{Real: new(0.0)}.Build()
		}
	}
	return out
}

// mstate is a state of the database in the model: its tables, by their folded names.
type mstate struct {
	tables map[string]*mtable
}

// index of the state with the given name, compared as SQLite compares names: its table and its
// place among the indexes of its table, or nil.
func (s *mstate) index(name string) (*mtable, int) {
	for _, t := range s.tables {
		for i, ci := range t.indexes {
			if foldName(ci.GetIndex()) == foldName(name) {
				return t, i
			}
		}
	}
	return nil, 0
}

func newState() *mstate {
	return &mstate{tables: map[string]*mtable{}}
}

// foldName as SQLite compares names: the ASCII letters in lowercase.
func foldName(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, name)
}

// clone the state. Rows are shared, because nothing changes a row.
func (s *mstate) clone() *mstate {
	c := newState()
	for name, t := range s.tables {
		rows := make(map[string][]*s4qlpb.Value, len(t.rows))
		for key, row := range t.rows {
			rows[key] = row
		}
		c.tables[name] = &mtable{create: t.create, rows: rows, indexes: slices.Clone(t.indexes)}
	}
	return c
}

// dump the state as [dump] dumps what a transaction reads.
func (s *mstate) dump() map[string][]string {
	state := map[string][]string{}
	for _, t := range s.tables {
		rows := []string{}
		for _, row := range t.sorted() {
			rows = append(rows, text(row))
		}
		state[describe(t.create)+describeIndexes(t.indexes)] = rows
	}
	return state
}

// text of the state, as [show] shows a dump.
func (s *mstate) text() string {
	return show(s.dump())
}

// mcommit is one commit object as the model reads it.
type mcommit struct {
	name   format.Name
	parent format.Name
	// lines of its changes and annotations, as [changes] gives them.
	lines []string
	// ids of the transactions that its annotations name.
	ids []uint64
}

// apply the changes of a commit object to the state, strictly: what a net effect never holds is an
// error. A net effect names a table in at most one drop and at most one create, the drop first; it
// lists a primary key of a table at most once; it deletes only rows that are there; and it changes
// no table that it drops and does not create again. It drops indexes before it changes a table,
// and only indexes that are there; it creates indexes after everything else, on tables and columns
// that are there, under names that no table and no index has; and it creates no table under the
// name of an index.
func (s *mstate) apply(name format.Name, data []byte) (*mcommit, error) {
	parent, lines, err := changes(name, data)
	if err != nil {
		return nil, err
	}
	c := &mcommit{name: name, parent: parent, lines: lines}

	type seen struct {
		dropped, created bool
		keys             map[string]bool
	}
	tables := map[string]*seen{}
	of := func(name string) *seen {
		folded := foldName(name)
		if tables[folded] == nil {
			tables[folded] = &seen{keys: map[string]bool{}}
		}
		return tables[folded]
	}

	// changedTable is whether the commit object has created or dropped a table or changed a row so
	// far, and createdIndex whether it has created an index.
	var changedTable, createdIndex bool

	r := format.NewCommitReader(strings.NewReader(string(data)), name)
	for {
		change, annotation, err := r.Next()
		if err != nil {
			// The end: changes read the whole commit object before.
			break
		}
		if annotation == nil && !change.HasCreateIndex() && createdIndex {
			return nil, errors.New("it changes something after it created an index")
		}
		if annotation == nil && !change.HasCreateIndex() && !change.HasDropIndex() {
			changedTable = true
		}
		switch {
		case annotation == nil && change.HasDropIndex():
			index := change.GetDropIndex().GetIndex()
			if changedTable {
				return nil, fmt.Errorf("it drops the index %q after it changed a table", index)
			}
			t, i := s.index(index)
			if t == nil {
				return nil, fmt.Errorf("it drops the index %q, which is not there", index)
			}
			t.indexes = slices.Delete(t.indexes, i, i+1)

		case annotation == nil && change.HasCreateIndex():
			ci := change.GetCreateIndex()
			createdIndex = true
			t := s.tables[foldName(ci.GetTable())]
			if t == nil {
				return nil, fmt.Errorf("it creates the index %q on the table %q, which is not there", ci.GetIndex(), ci.GetTable())
			}
			if other, _ := s.index(ci.GetIndex()); other != nil || s.tables[foldName(ci.GetIndex())] != nil {
				return nil, fmt.Errorf("it creates the index %q, and a table or an index has the name", ci.GetIndex())
			}
			for _, c := range ci.GetColumns() {
				if int(c.GetColumn()) >= len(t.create.GetColumns()) {
					return nil, fmt.Errorf("it creates the index %q on column %v of the table %q", ci.GetIndex(), c.GetColumn(), ci.GetTable())
				}
			}
			t.indexes = append(t.indexes, ci)

		case annotation != nil:
			var id uint64
			if _, err := fmt.Sscanf(annotation.GetStatement().GetSql(), "transaction %d", &id); err == nil {
				c.ids = append(c.ids, id)
			}

		case change.HasDropTable():
			table := change.GetDropTable().GetTable()
			seen := of(table)
			if seen.dropped || seen.created || len(seen.keys) > 0 {
				return nil, fmt.Errorf("it drops the table %q after it dropped, created, or changed it", table)
			}
			seen.dropped = true
			if s.tables[foldName(table)] == nil {
				return nil, fmt.Errorf("it drops the table %q, which is not there", table)
			}
			delete(s.tables, foldName(table))

		case change.HasCreateTable():
			ct := change.GetCreateTable()
			seen := of(ct.GetTable())
			if seen.created || len(seen.keys) > 0 {
				return nil, fmt.Errorf("it creates the table %q after it created or changed it", ct.GetTable())
			}
			seen.created = true
			if s.tables[foldName(ct.GetTable())] != nil {
				return nil, fmt.Errorf("it creates the table %q, which is there", ct.GetTable())
			}
			if other, _ := s.index(ct.GetTable()); other != nil {
				return nil, fmt.Errorf("it creates the table %q, and an index has the name", ct.GetTable())
			}
			s.tables[foldName(ct.GetTable())] = &mtable{create: ct, rows: map[string][]*s4qlpb.Value{}}

		case change.HasUpsertRows():
			ur := change.GetUpsertRows()
			t := s.tables[foldName(ur.GetTable())]
			if t == nil {
				return nil, fmt.Errorf("it upserts rows of the table %q, which is not there", ur.GetTable())
			}
			if !slices.Equal(ur.GetColumns(), columnPositions(len(t.create.GetColumns()))) {
				return nil, fmt.Errorf("it upserts the columns %v of the table %q", ur.GetColumns(), ur.GetTable())
			}
			seen := of(ur.GetTable())
			for _, row := range ur.GetRows() {
				for i, v := range row.GetValues() {
					if err := fits(t.create.GetColumns()[i], v); err != nil {
						return nil, fmt.Errorf("it upserts into the table %q %v", ur.GetTable(), err)
					}
				}
				key := text(t.key(row.GetValues()))
				if seen.keys[key] {
					return nil, fmt.Errorf("it lists the key %v of the table %q twice", key, ur.GetTable())
				}
				seen.keys[key] = true
				t.rows[key] = row.GetValues()
			}

		case change.HasDeleteRows():
			dr := change.GetDeleteRows()
			t := s.tables[foldName(dr.GetTable())]
			if t == nil {
				return nil, fmt.Errorf("it deletes rows of the table %q, which is not there", dr.GetTable())
			}
			seen := of(dr.GetTable())
			if seen.created {
				return nil, fmt.Errorf("it deletes rows of the table %q, which it created", dr.GetTable())
			}
			for _, pk := range dr.GetPrimaryKeys() {
				key := text(pk.GetValues())
				if seen.keys[key] {
					return nil, fmt.Errorf("it lists the key %v of the table %q twice", key, dr.GetTable())
				}
				seen.keys[key] = true
				if t.rows[key] == nil {
					return nil, fmt.Errorf("it deletes the row %v of the table %q, which is not there", key, dr.GetTable())
				}
				delete(t.rows, key)
			}
		}
	}
	return c, nil
}

// fits reports why the value v does not fit the column c, or nil if it does: a value of the kind of
// the column's type, or NULL if the column takes NULL.
func fits(c *s4qlpb.Column, v *s4qlpb.Value) error {
	switch {
	case v.WhichKind() == s4qlpb.Value_Null_case && c.GetNullable():
		return nil
	case v.WhichKind() == s4qlpb.Value_Null_case:
		return fmt.Errorf("NULL in the column %q, which does not take NULL", c.GetName())
	// The edges of a type are values of its kind.
	case len(edges[c.GetType()]) == 0 || v.WhichKind() != edges[c.GetType()][0].WhichKind():
		return fmt.Errorf("a value of kind %v in the column %q of type %v", v.WhichKind(), c.GetName(), c.GetType())
	}
	return nil
}

func columnPositions(n int) []uint32 {
	positions := make([]uint32, n)
	for i := range positions {
		positions[i] = uint32(i)
	}
	return positions
}

// mtx is a write transaction in the model: the state as the transaction sees it, and what it has
// done to each table, from which the model says what its commit object must hold. It is the
// model's own account of the net effect, kept in maps.
type mtx struct {
	// before is the state when the transaction began, and now the state as it sees it.
	before, now *mstate
	// order of the folded names of the tables that the transaction changed, by first change.
	order   []string
	changed map[string]*mchanged
	// droppedIndexes are the names of the indexes that were there when the transaction began and
	// that it dropped by their names, in that order, and createdIndexes the folded names of the
	// indexes that it created and that are there, in the order in which it created them.
	droppedIndexes, createdIndexes []string
}

type mchanged struct {
	// dropped is the name of the table that was there when the transaction began, if it dropped it.
	dropped string
	// created is whether the transaction created the table that is there now.
	created bool
	// touched are the keys of the rows that the transaction upserted or deleted in a table that was
	// there when it began.
	touched map[string][]*s4qlpb.Value
}

func newTx(before *mstate) *mtx {
	return &mtx{before: before, now: before.clone(), changed: map[string]*mchanged{}}
}

// clone the transaction, as a savepoint keeps it. The state before is shared, because nothing
// changes it.
func (m *mtx) clone() *mtx {
	c := &mtx{before: m.before, now: m.now.clone(), order: slices.Clone(m.order), changed: make(map[string]*mchanged, len(m.changed)),
		droppedIndexes: slices.Clone(m.droppedIndexes), createdIndexes: slices.Clone(m.createdIndexes)}
	for folded, ch := range m.changed {
		c.changed[folded] = &mchanged{dropped: ch.dropped, created: ch.created, touched: maps.Clone(ch.touched)}
	}
	return c
}

func (m *mtx) change(name string) *mchanged {
	folded := foldName(name)
	c, ok := m.changed[folded]
	if !ok {
		c = &mchanged{touched: map[string][]*s4qlpb.Value{}}
		m.changed[folded] = c
		m.order = append(m.order, folded)
	}
	return c
}

// taken is the error of creating a table or an index with the given name, which a table and an
// index share: one that says that a table has it, or that an index has it, or nil.
func (m *mtx) taken(name string) error {
	if m.now.tables[foldName(name)] != nil {
		return core.ErrTableExists
	}
	if t, _ := m.now.index(name); t != nil {
		return core.ErrIndexExists
	}
	return nil
}

// createIndex on a table that is there, and return nil, or the error of [mtx.taken] if a table or
// an index has its name.
func (m *mtx) createIndex(ci *s4qlpb.CreateIndex) error {
	if err := m.taken(ci.GetIndex()); err != nil {
		return err
	}
	t := m.now.tables[foldName(ci.GetTable())]
	t.indexes = append(t.indexes, ci)
	m.createdIndexes = append(m.createdIndexes, foldName(ci.GetIndex()))
	return nil
}

// dropIndex with the given name, and report whether there was one.
func (m *mtx) dropIndex(name string) bool {
	t, i := m.now.index(name)
	if t == nil {
		return false
	}
	if at := slices.Index(m.createdIndexes, foldName(name)); at >= 0 {
		m.createdIndexes = slices.Delete(m.createdIndexes, at, at+1)
	} else {
		m.droppedIndexes = append(m.droppedIndexes, t.indexes[i].GetIndex())
	}
	t.indexes = slices.Delete(t.indexes, i, i+1)
	return true
}

// create the table, and report whether no table and no index had the name.
func (m *mtx) create(ct *s4qlpb.CreateTable) bool {
	if m.taken(ct.GetTable()) != nil {
		return false
	}
	m.now.tables[foldName(ct.GetTable())] = &mtable{create: ct, rows: map[string][]*s4qlpb.Value{}}
	m.change(ct.GetTable()).created = true
	return true
}

// drop the table, and report whether there was one.
func (m *mtx) drop(name string) bool {
	t := m.now.tables[foldName(name)]
	if t == nil {
		return false
	}
	c := m.change(name)
	if c.created {
		c.created = false
	} else {
		c.dropped = t.create.GetTable()
	}
	clear(c.touched)
	// The indexes of the table go with it.
	for _, ci := range t.indexes {
		m.createdIndexes = slices.DeleteFunc(m.createdIndexes, func(folded string) bool { return folded == foldName(ci.GetIndex()) })
	}
	delete(m.now.tables, foldName(name))
	return true
}

// upsert the row, as a column holds it, and report whether the table is there.
func (m *mtx) upsert(name string, row []*s4qlpb.Value) bool {
	t := m.now.tables[foldName(name)]
	if t == nil {
		return false
	}
	row = stored(row)
	key := t.key(row)
	if c := m.change(name); !c.created {
		c.touched[text(key)] = key
	}
	t.rows[text(key)] = row
	return true
}

// remove the row with the key, and report whether the table is there and whether the row was.
func (m *mtx) remove(name string, key []*s4qlpb.Value) (table, found bool) {
	t := m.now.tables[foldName(name)]
	if t == nil {
		return false, false
	}
	key = stored(key)
	if c := m.change(name); !c.created {
		c.touched[text(key)] = key
	}
	found = t.rows[text(key)] != nil
	delete(t.rows, text(key))
	return true, found
}

// effect is the net effect of the transaction, as the lines of [changes] for its commit object
// without the annotations.
func (m *mtx) effect() []string {
	var lines []string
	for _, name := range m.droppedIndexes {
		lines = append(lines, "drop index "+name)
	}
	for _, folded := range m.order {
		c := m.changed[folded]
		if c.dropped != "" {
			lines = append(lines, "drop "+c.dropped)
		}
		t := m.now.tables[folded]
		if t == nil {
			continue
		}
		columns := columnPositions(len(t.create.GetColumns()))
		if c.created {
			lines = append(lines, "create "+describe(t.create))
			for _, row := range t.sorted() {
				lines = append(lines, fmt.Sprint("upsert ", t.create.GetTable(), " ", columns, " ", text(row)))
			}
			continue
		}
		keys := make([][]*s4qlpb.Value, 0, len(c.touched))
		for _, key := range c.touched {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, compareKeys)
		for _, key := range keys {
			if row := t.rows[text(key)]; row != nil {
				lines = append(lines, fmt.Sprint("upsert ", t.create.GetTable(), " ", columns, " ", text(row)))
			}
		}
		was := m.before.tables[folded]
		for _, key := range keys {
			if t.rows[text(key)] == nil && was != nil && was.rows[text(key)] != nil {
				lines = append(lines, fmt.Sprint("delete ", t.create.GetTable(), " ", text(key)))
			}
		}
	}
	for _, folded := range m.createdIndexes {
		t, i := m.now.index(folded)
		lines = append(lines, "create index "+describeIndex(t.indexes[i]))
	}
	return lines
}

// model of the log of one database in a simulated store, read through the store's direct view.
type model struct {
	store  *simstore.Store
	prefix string

	mu sync.Mutex
	// chain of the commit objects of the log, oldest first, and the state after each. The state at
	// index 0 is the one of the empty log, so the commit object at chain[i] gives states[i+1].
	chain  []*mcommit
	states []*mstate
	// index in states of the state after each commit object, by its name. The empty log is at 0.
	index map[format.Name]int
	// commit object of each transaction, by the ID in its annotation.
	byID map[uint64]int
	// broken is the first thing that is wrong with the log, after which the model reads no further.
	broken error
}

func newModel(store *simstore.Store, prefix string) *model {
	return &model{
		store:  store,
		prefix: prefix,
		states: []*mstate{newState()},
		index:  map[format.Name]int{format.EmptyLog: 0},
		byID:   map[uint64]int{},
	}
}

// sync the model with the log as the store has it now. It fails if the log is not one that the
// rules allow: the head of the manifest is not a commit object on top of the log so far, or a
// commit object is not a net effect.
func (m *model) sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncLocked()
}

func (m *model) syncLocked() error {
	if m.broken != nil {
		return m.broken
	}
	data, _, ok := m.store.Object(m.prefix + "/manifest.json")
	if !ok {
		return nil
	}
	man, err := format.ParseManifest(data)
	if err != nil {
		m.broken = fmt.Errorf("the manifest: %w", err)
		return m.broken
	}
	if err := m.extend(man.Head); err != nil {
		m.broken = err
	}
	return m.broken
}

// extend the chain up to the commit object head, which must lead back to the newest one that the
// model has.
func (m *model) extend(head format.Name) error {
	if _, ok := m.index[head]; ok {
		if m.index[head] != len(m.states)-1 {
			return fmt.Errorf("the head of the log went back to commit %v, from commit %v", m.index[head], len(m.states)-1)
		}
		return nil
	}
	last := format.EmptyLog
	if len(m.chain) > 0 {
		last = m.chain[len(m.chain)-1].name
	}
	// The commit objects between the newest one that the model has and the head, newest first.
	type object struct {
		name format.Name
		data []byte
	}
	var missing []object
	for name := head; name != last; {
		if name == format.EmptyLog {
			return fmt.Errorf("the log that ends at %v does not hold its commit %v, %v", head, len(m.chain), last)
		}
		data, _, ok := m.store.Object(commitKey(m.prefix, name))
		if !ok {
			return fmt.Errorf("the log names the commit object %v, which the store does not have", name)
		}
		parent, _, err := changes(name, data)
		if err != nil {
			return fmt.Errorf("the commit object %v: %w", name, err)
		}
		missing = append(missing, object{name, data})
		name = parent
	}
	for _, o := range slices.Backward(missing) {
		state := m.states[len(m.states)-1].clone()
		c, err := state.apply(o.name, o.data)
		if err != nil {
			return fmt.Errorf("commit %v, %v, is no net effect: %w", len(m.states), o.name, err)
		}
		m.chain = append(m.chain, c)
		m.states = append(m.states, state)
		m.index[o.name] = len(m.states) - 1
		for _, id := range c.ids {
			if at, ok := m.byID[id]; ok {
				return fmt.Errorf("the log holds the transaction %v twice, as commit %v and as commit %v", id, at, len(m.states)-1)
			}
			m.byID[id] = len(m.states) - 1
		}
	}
	return nil
}

// at returns the number of the commit with the given name in the log, from 1, or 0 for the empty
// log, and the state after it. It reports false if the log does not hold it.
func (m *model) at(name format.Name) (int, *mstate, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.syncLocked()
	i, ok := m.index[name]
	if !ok {
		return 0, nil, false
	}
	return i, m.states[i], true
}

// of returns the number of the commit of the transaction with the given ID, and its commit object,
// or false if the log does not hold it.
func (m *model) of(id uint64) (int, *mcommit, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.syncLocked()
	i, ok := m.byID[id]
	if !ok {
		return 0, nil, false
	}
	return i, m.chain[i-1], true
}

// head is the number of the newest commit of the log.
func (m *model) head() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.syncLocked()
	return len(m.states) - 1
}

// commitKey of the commit object with the given name in the database under the prefix: in a
// folder named by the first two hexadecimal digits of the name, under the whole name.
func commitKey(prefix string, name format.Name) string {
	s := name.String()
	return prefix + "/commits/" + s[:2] + "/" + s
}

// cacheDump reads the cache at path with a connection of its own and SQL of its own: its recorded
// position, and its tables, with their indexes, and rows as [dump] gives them. It fails if SQLite's
// integrity check finds a fault in the cache, such as an index that does not hold what its table
// holds.
func cacheDump(path string) (position format.Name, state map[string][]string, err error) {
	c, err := sqlite.Open(path)
	if err != nil {
		return format.Name{}, nil, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()

	query := func(sql string, row func(s *sqlite.Stmt)) error {
		s, err := c.Prepare(sql)
		if err != nil {
			return err
		}
		for {
			ok, err := s.Step()
			if err != nil {
				_ = s.Finalize()
				return err
			}
			if !ok {
				return s.Finalize()
			}
			row(s)
		}
	}
	quoted := func(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
	literal := func(name string) string { return `'` + strings.ReplaceAll(name, `'`, `''`) + `'` }

	var head string
	if err := query("select head from s4ql_cache", func(s *sqlite.Stmt) { head = s.ColumnText(0) }); err != nil {
		return format.Name{}, nil, err
	}
	if position, err = format.ParseName(head); err != nil {
		return format.Name{}, nil, err
	}

	var faults []string
	if err := query("pragma integrity_check", func(s *sqlite.Stmt) {
		if fault := s.ColumnText(0); fault != "ok" {
			faults = append(faults, fault)
		}
	}); err != nil {
		return format.Name{}, nil, err
	}
	if len(faults) > 0 {
		return format.Name{}, nil, fmt.Errorf("the integrity check of the cache at %v: %v", path, strings.Join(faults, "; "))
	}

	var names []string
	if err := query("select name from sqlite_schema where type = 'table'", func(s *sqlite.Stmt) { names = append(names, s.ColumnText(0)) }); err != nil {
		return format.Name{}, nil, err
	}
	state = map[string][]string{}
	for _, name := range names {
		if strings.HasPrefix(name, "s4ql_") {
			continue
		}
		ct := s4qlpb.CreateTable_builder{Table: new(name)}
		keyed := map[int64]uint32{}
		// A column that takes NULL is declared "default null".
		if err := query("select name, type, pk, coalesce(lower(dflt_value) = 'null', 0) from pragma_table_info("+literal(name)+") order by cid", func(s *sqlite.Stmt) {
			if pk := s.ColumnInt64(2); pk > 0 {
				keyed[pk] = uint32(len(ct.Columns))
			}
			// SQLite reports the type of a column of a strict table in capitals.
			typ := s4qlpb.Type_TYPE_UNSPECIFIED
			for t, n := range typeNames {
				if strings.ToUpper(n) == s.ColumnText(1) {
					typ = t
				}
			}
			if typ == s4qlpb.Type_TYPE_UNSPECIFIED {
				err = fmt.Errorf("the column %v of the table %v has the type %v", s.ColumnText(0), name, s.ColumnText(1))
			}
			column := s4qlpb.Column_builder{Name: new(s.ColumnText(0)), Type: new(typ)}
			if s.ColumnInt64(3) == 1 {
				column.Nullable = new(true)
			}
			ct.Columns = append(ct.Columns, column.Build())
		}); err != nil {
			return format.Name{}, nil, err
		}
		var order []string
		for i := range len(keyed) {
			ct.PrimaryKey = append(ct.PrimaryKey, keyed[int64(i+1)])
			order = append(order, quoted(ct.Columns[keyed[int64(i+1)]].GetName()))
		}
		rows := []string{}
		if err := query("select * from "+quoted(name)+" order by "+strings.Join(order, ", "), func(s *sqlite.Stmt) {
			values := make([]*s4qlpb.Value, s.ColumnCount())
			for i := range values {
				values[i] = columnValue(s, i)
			}
			rows = append(rows, text(values))
		}); err != nil {
			return format.Name{}, nil, err
		}

		// A connection reads the schema in the order of its rows and puts each index before those that
		// it read earlier, so SQLite lists the indexes of a table from the newest to the oldest. This
		// is another way to their order than the rowids that the core reads.
		var indexNames []string
		if err := query("select name from pragma_index_list("+literal(name)+") where origin = 'c'", func(s *sqlite.Stmt) {
			indexNames = append(indexNames, s.ColumnText(0))
		}); err != nil {
			return format.Name{}, nil, err
		}
		slices.Reverse(indexNames)
		var indexes []*s4qlpb.CreateIndex
		for _, index := range indexNames {
			ci := s4qlpb.CreateIndex_builder{Index: new(index), Table: new(name)}
			if err := query(`select cid, "desc" from pragma_index_xinfo(`+literal(index)+`) where key order by seqno`, func(s *sqlite.Stmt) {
				column := s4qlpb.IndexColumn_builder{Column: new(uint32(s.ColumnInt64(0)))}
				if s.ColumnInt64(1) != 0 {
					column.Descending = new(true)
				}
				ci.Columns = append(ci.Columns, column.Build())
			}); err != nil {
				return format.Name{}, nil, err
			}
			indexes = append(indexes, ci.Build())
		}
		state[describe(ct.Build())+describeIndexes(indexes)] = rows
	}
	return position, state, err
}

// types of a column, in the order of their values in the commit object.
var types = []s4qlpb.Type{s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB}

// edges are values of each type of column, in no order, that exercise the edges of SQLite's order
// and of the encodings: integers at the limits of 64 bits; reals that are negative, tiny, huge,
// below the smallest normal number, 0.0, and -0.0, which is the same key as 0.0; the empty text,
// text with multi-byte characters, text that differs from other text only in a later byte, and text
// that is a prefix of other text; and the empty blob, blobs with zero bytes, and blobs that are a
// prefix of other blobs.
var edges = map[s4qlpb.Type][]*s4qlpb.Value{
	s4qlpb.Type_TYPE_INT:     row(math.MinInt64, math.MinInt64+1, -1, 0, 1, 1<<53+1, math.MaxInt64-1, math.MaxInt64),
	s4qlpb.Type_TYPE_INTEGER: row(math.MaxInt64, -(1 << 31), 1<<31, 0, math.MinInt64, 255, 256),
	s4qlpb.Type_TYPE_REAL: row(-math.MaxFloat64, -1e300, -1.5, -math.SmallestNonzeroFloat64, 0.0, math.Copysign(0, -1),
		math.SmallestNonzeroFloat64, 2.2250738585072009e-308, 0.1, 1.0, 1.5, 1<<53+2.0, 1e300, math.MaxFloat64),
	s4qlpb.Type_TYPE_TEXT: row("", "a", "aa", "ab", "A", "Z", "~", "a\x01", "a\x7f", "é", "è", "é", "aé", "aè", "日本", "日本語",
		"🥪", "\U0010ffff", "ÿ", strings.Repeat("x", 300), strings.Repeat("x", 300)+"y"),
	s4qlpb.Type_TYPE_BLOB: row([]byte{}, []byte{0}, []byte{0, 0}, []byte{0, 1}, []byte{1}, []byte{0xff}, []byte{0xff, 0},
		[]byte("ab"), []byte("abc"), []byte("abd"), []byte{'a', 0, 'b'}, []byte{'a', 0, 'c'}),
}

// columnValue i of the current row of s.
func columnValue(s *sqlite.Stmt, i int) *s4qlpb.Value {
	switch s.ColumnType(i) {
	case sqlite.TypeInteger:
		return s4qlpb.Value_builder{Integer: new(s.ColumnInt64(i))}.Build()
	case sqlite.TypeFloat:
		return s4qlpb.Value_builder{Real: new(s.ColumnDouble(i))}.Build()
	case sqlite.TypeText:
		return s4qlpb.Value_builder{Text: new(s.ColumnText(i))}.Build()
	case sqlite.TypeBlob:
		return s4qlpb.Value_builder{Blob: s.ColumnBlob(i)}.Build()
	default:
		return s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
	}
}

// bindValue v to the parameter i of s, as it is.
func bindValue(s *sqlite.Stmt, i int, v *s4qlpb.Value) error {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return s.BindInt64(i, v.GetInteger())
	case s4qlpb.Value_Real_case:
		return s.BindDouble(i, v.GetReal())
	case s4qlpb.Value_Text_case:
		return s.BindText(i, v.GetText())
	case s4qlpb.Value_Blob_case:
		return s.BindBlob(i, v.GetBlob())
	default:
		return s.BindNull(i)
	}
}
