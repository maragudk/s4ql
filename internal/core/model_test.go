package core_test

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

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

// compareKeys of integers, as SQLite orders them.
func compareKeys(a, b []*s4qlpb.Value) int {
	for i := range a {
		if c := cmp.Compare(a[i].GetInteger(), b[i].GetInteger()); c != 0 {
			return c
		}
	}
	return 0
}

// mstate is a state of the database in the model: its tables, by their folded names.
type mstate struct {
	tables map[string]*mtable
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
		c.tables[name] = &mtable{create: t.create, rows: rows}
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
		state[describe(t.create)] = rows
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
// no table that it drops and does not create again.
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

	r := format.NewCommitReader(strings.NewReader(string(data)), name)
	for {
		change, annotation, err := r.Next()
		if err != nil {
			// The end: changes read the whole commit object before.
			break
		}
		switch {
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

// create the table, and report whether none had the name.
func (m *mtx) create(ct *s4qlpb.CreateTable) bool {
	if m.now.tables[foldName(ct.GetTable())] != nil {
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
	delete(m.now.tables, foldName(name))
	return true
}

// upsert the row, and report whether the table is there.
func (m *mtx) upsert(name string, row []*s4qlpb.Value) bool {
	t := m.now.tables[foldName(name)]
	if t == nil {
		return false
	}
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
// position, and its tables and rows as [dump] gives them.
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
		if err := query("select name, type, pk from pragma_table_info("+literal(name)+") order by cid", func(s *sqlite.Stmt) {
			if pk := s.ColumnInt64(2); pk > 0 {
				keyed[pk] = uint32(len(ct.Columns))
			}
			if s.ColumnText(1) != "INT" {
				err = fmt.Errorf("the column %v of the table %v has the type %v", s.ColumnText(0), name, s.ColumnText(1))
			}
			ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(s.ColumnText(0)), Type: new(s4qlpb.Type_TYPE_INT)}.Build())
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
			values := make([]string, s.ColumnCount())
			for i := range values {
				switch s.ColumnType(i) {
				case sqlite.TypeInteger:
					values[i] = fmt.Sprint(s.ColumnInt64(i))
				case sqlite.TypeNull:
					values[i] = "null"
				default:
					values[i] = fmt.Sprint("?", s.ColumnType(i))
				}
			}
			rows = append(rows, strings.Join(values, ","))
		}); err != nil {
			return format.Name{}, nil, err
		}
		state[describe(ct.Build())] = rows
	}
	return position, state, err
}
