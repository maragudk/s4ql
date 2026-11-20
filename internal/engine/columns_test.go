package engine_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// FuzzColumns runs, on the engine on a database in memory, a test double of the core, and on SQLite,
// a table of several columns and a run of inserts and selects on it, all of which the bytes of its
// input choose as [columnsCalls] says, and compares what each gives, as [TestDifferential] does,
// also with every row read: see [pair.bothWays]. Then it compares the tables. [FuzzColumns_core]
// runs the same on the storage core. The database in memory has no length limit, so the input of
// the limit that [FuzzColumns_core] reads changes nothing here.
func FuzzColumns(f *testing.F) {
	fuzzColumns(f, func(t testing.TB, limit int) *instance { return newMemoryInstance(t) })
}

// FuzzColumns_core is [FuzzColumns] on the storage core, with a store in memory and the length limit
// that its input chooses, from 1001 to 1255 bytes or SQLite's default, in the cache and in SQLite.
// After the calls, it replays the log into an empty cache and compares the tables again.
func FuzzColumns_core(f *testing.F) {
	fuzzColumns(f, func(t testing.TB, limit int) *instance { return newInstanceLimited(t, engine.Options{}, nil, limit) })
}

// fuzzColumns is [FuzzColumns] on the instances of newInstance, which takes the length limit, 0 for
// SQLite's default. Its seeds are the inputs of [columnsSeeds].
func fuzzColumns(f *testing.F, newInstance func(t testing.TB, limit int) *instance) {
	for _, seed := range columnsSeeds() {
		f.Add(seed.data, seed.limit)
	}
	f.Fuzz(func(t *testing.T, data []byte, limit uint8) {
		n := 0
		if limit > 0 {
			n = 1000 + int(limit)
		}
		i := newInstance(t, n)
		p := newPair(t, i, nil)
		var log []string
		for _, c := range columnsCalls(data) {
			log = append(log, fmt.Sprintf("%v -- with %#v", c.Src, c.Args))
			if failure := p.step(c); failure != "" {
				t.Fatalf("after\n%v\n%v", strings.Join(log[:len(log)-1], "\n"), failure)
			}
		}
		want := p.o.state(t)
		if got := i.state(); got != want {
			t.Fatalf("after\n%v\nthe engine holds\n%v\nand SQLite\n%v", strings.Join(log, "\n"), got, want)
		}
		if i.memory != nil {
			return
		}
		i.restart(true, engine.Options{})
		if got := i.state(); got != want {
			t.Fatalf("after\n%v\nthe engine holds after a replay into an empty cache\n%v\nand SQLite\n%v", strings.Join(log, "\n"), got, want)
		}
	})
}

// columnsSeed is an input of [FuzzColumns]: its bytes and its length limit.
type columnsSeed struct {
	data  []byte
	limit uint8
}

// columnsSeeds of [FuzzColumns]: for each of five shapes, inputs that reach each form of
// [columnsCalls] by hand, after four rows: each list of columns, NULL in a column that takes it and
// in one that does not, each fault, a value of another type in each column, each kind of select, a
// WHERE on each column, and values about the lowered length limit of 1001 bytes, where a value or a
// record of several is too long. And inputs of random bytes from fixed seeds, half of them at that
// limit.
func columnsSeeds() []columnsSeed {
	shapes := []struct {
		shape               columnsSeedShape
		options, keyNotNull byte
	}{
		// Two columns: an INT key that says NOT NULL after PRIMARY KEY, and TEXT that takes NULL.
		{columnsSeedShape{types: []int{0, 3}, nullable: []bool{false, true}, key: 0}, 0, 8},
		// Three columns: REAL that does not take NULL, a BLOB key, and INTEGER that takes NULL; the
		// options in the other order.
		{columnsSeedShape{types: []int{2, 4, 1}, nullable: []bool{false, false, true}, key: 1}, 1, 0},
		// Four columns: TEXT and BLOB that take NULL, INT that does not, and a TEXT key that says NOT
		// NULL before PRIMARY KEY.
		{columnsSeedShape{types: []int{3, 4, 0, 3}, nullable: []bool{true, true, false, false}, key: 3}, 0, 16},
		// Four INT columns, none of which takes NULL.
		{columnsSeedShape{types: []int{0, 0, 0, 0}, nullable: []bool{false, false, false, false}, key: 1}, 1, 0},
		// Three columns: TEXT that does not take NULL, an INT key, and BLOB that takes NULL, so that two
		// values beside a key of a few bytes make a record that is too long.
		{columnsSeedShape{types: []int{3, 0, 4}, nullable: []bool{false, false, true}, key: 1}, 0, 0},
	}
	var seeds []columnsSeed
	for _, sh := range shapes {
		s := sh.shape
		head := slices.Concat(s.bytes(sh.options, sh.keyNotNull), s.rows())
		for _, group := range [][][]byte{s.lists(), s.faults(), s.typesAndSelects(), s.wheres()} {
			seeds = append(seeds, columnsSeed{data: slices.Concat(head, slices.Concat(group...))})
		}
		if long := s.long(); long != nil {
			seeds = append(seeds, columnsSeed{data: slices.Concat(head, slices.Concat(long...)), limit: 1})
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	for j := range 40 {
		data := make([]byte, 20+r.IntN(80))
		for k := range data {
			data[k] = byte(r.Uint32())
		}
		seeds = append(seeds, columnsSeed{data: data, limit: uint8(j % 2)})
	}
	return seeds
}

// columnsSeedShape is a table of a seed of [FuzzColumns]: the index in [whereTypes] of the type of
// each column, which of them take NULL, and the index of the key.
type columnsSeedShape struct {
	types    []int
	nullable []bool
	key      int
}

// bytes of the create table of the shape, as [columnsCalls] reads them, with the options in the
// order of options, and NOT NULL on the key as keyNotNull, 0, 8, or 16, says.
func (s columnsSeedShape) bytes(options, keyNotNull byte) []byte {
	b := []byte{byte(len(s.types) - 2), byte(s.key)}
	for j, typ := range s.types {
		c := byte(typ)
		switch {
		case j == s.key:
			c |= keyNotNull
		case s.nullable[j]:
			c |= 8
		}
		b = append(b, c)
	}
	return append(b, options)
}

// The bytes of a value of [columnsValue]: a value of the pool by its index, NULL, an edge of the
// type by its index, and, with arg, as an argument.
func seedPool(i int, arg bool) []byte { return []byte{byte(i<<4) | seedArg(arg)} }
func seedNull(arg bool) []byte        { return []byte{4 | seedArg(arg)} }
func seedEdge(i int, arg bool) []byte { return []byte{7 | byte(i<<4) | seedArg(arg)} }

func seedArg(arg bool) byte {
	if arg {
		return 8
	}
	return 0
}

// newKey is the bytes of the k-th of the values of the type of the key of the shape that no row of
// [columnsSeedShape.rows] holds, from bytes as [whereValue] takes them, and, with arg, as an
// argument.
func (s columnsSeedShape) newKey(k int, arg bool) []byte {
	b := []byte{2 | seedArg(arg)}
	switch whereTypes[s.types[s.key]] {
	case "text", "blob":
		return append(b, 2, 'k', byte('0'+k))
	case "real":
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(float64(100+k)))
	}
	return binary.LittleEndian.AppendUint64(b, uint64(100+k))
}

// seedOther is the bytes of a value of another type than typ, the i-th of the others, with the
// bytes that [whereValue] takes for it.
func seedOther(typ string, i int, arg bool) []byte {
	others := slices.DeleteFunc(slices.Clone(whereTypes), func(o string) bool {
		return o == typ || strings.HasPrefix(o, "int") && strings.HasPrefix(typ, "int")
	})
	b := []byte{5 | byte(i<<4) | seedArg(arg)}
	switch others[i%len(others)] {
	case "text", "blob":
		return append(b, 2, 'z', 'y')
	}
	return append(b, 3, 0, 0, 0, 0, 0, 0xf8, 0x3f)
}

// seedShuffled is the list of the columns 0 to n-1 as [columnsInsert] shuffles it with every byte
// b.
func seedShuffled(list []int, b byte) []int {
	list = slices.Clone(list)
	for j := len(list) - 1; j > 0; j-- {
		k := int(b) % (j + 1)
		list[j], list[k] = list[k], list[j]
	}
	return list
}

// all the columns of the shape, in its order.
func (s columnsSeedShape) all() []int {
	var list []int
	for j := range s.types {
		list = append(list, j)
	}
	return list
}

// row r of the shape, for the columns of list in its order, as values of the pool, which differ
// in the key for r from 0 to 3.
func (s columnsSeedShape) row(list []int, r int) []byte {
	var b []byte
	for _, j := range list {
		i := (r + j) % 4
		if j == s.key {
			i = r % 4
		}
		b = append(b, seedPool(i, (r+j)%2 == 1)...)
	}
	return b
}

// rows of the shape: four inserts without a list, of four keys.
func (s columnsSeedShape) rows() []byte {
	var b []byte
	for r := range 4 {
		b = append(b, 0, 0)
		b = append(b, s.row(s.all(), r)...)
	}
	return b
}

// lists of the shape: an insert of a key that is there, then one with each kind of list, with NULL
// in each column that takes it, as a literal and as an argument, an insert that leaves out the
// columns that take NULL, one that leaves out every column but the key, and NULL in the key and in
// a column that does not take it.
func (s columnsSeedShape) lists() [][]byte {
	calls := [][]byte{slices.Concat([]byte{0, 0}, s.row(s.all(), 0))}
	value := func(j int, key []byte, arg bool) []byte {
		switch {
		case j == s.key:
			return key
		case s.nullable[j]:
			return seedNull(arg)
		}
		return seedPool(j%4, arg)
	}
	// The list in the order of the table, with names in upper case and in double quotes.
	c := []byte{1 << 1, 0b01100110}
	for _, j := range s.all() {
		c = append(c, value(j, s.newKey(0, false), false)...)
	}
	calls = append(calls, c)
	// The list shuffled, by bytes 0, which move it for every count of columns.
	c = []byte{2 << 1, 0b10011001}
	for range len(s.types) - 1 {
		c = append(c, 0)
	}
	for _, j := range seedShuffled(s.all(), 0) {
		c = append(c, value(j, s.newKey(1, true), true)...)
	}
	calls = append(calls, c)
	// A part of the list: the key and the columns that do not take NULL, and the key alone.
	var mask byte
	var list []int
	for j, nullable := range s.nullable {
		if !nullable {
			mask |= 1 << j
			list = append(list, j)
		}
	}
	c = []byte{3 << 1, 0, mask}
	for range len(list) - 1 {
		c = append(c, 0)
	}
	for _, j := range seedShuffled(list, 0) {
		c = append(c, value(j, s.newKey(2, false), false)...)
	}
	calls = append(calls, c, slices.Concat([]byte{3 << 1, 0, 1 << s.key}, s.newKey(3, true)))
	// NULL in the key, and in the first column that does not take it.
	c = []byte{0, 0}
	for _, j := range s.all() {
		if j == s.key {
			c = append(c, seedNull(false)...)
			continue
		}
		c = append(c, seedPool(1, false)...)
	}
	calls = append(calls, c)
	if j := slices.IndexFunc(s.all(), func(j int) bool { return j != s.key && !s.nullable[j] }); j >= 0 {
		c = []byte{0, 0}
		for _, k := range s.all() {
			switch k {
			case s.key:
				c = append(c, s.newKey(4, false)...)
			case j:
				c = append(c, seedNull(true)...)
			default:
				c = append(c, seedPool(2, false)...)
			}
		}
		calls = append(calls, c)
	}
	return append(calls, []byte{1})
}

// faults of the shape: an insert with the list in the order of the table and a value more, a value
// fewer, a name x that the table does not have, and a name twice.
func (s columnsSeedShape) faults() [][]byte {
	n := len(s.types)
	more := slices.Concat([]byte{1<<3 | 1<<1, 0, 0}, s.row(s.all(), 5), seedPool(1, false))
	fewer := slices.Concat([]byte{2<<3 | 1<<1, 0}, s.row(s.all()[:n-1], 6))
	x := slices.Concat([]byte{3<<3 | 1<<1, 0, 0}, s.row(s.all(), 7))
	twice := slices.Concat([]byte{4<<3 | 1<<1, 0, 1}, s.row(s.all(), 8), seedPool(1, false))
	return [][]byte{more, fewer, x, twice, {1}}
}

// typesAndSelects of the shape: an insert with a value of another type in each column, as a literal
// and as an argument, and with an argument that no column holds; and selects of names in other
// spellings, of a name twice, and of x, in and out of double quotes.
func (s columnsSeedShape) typesAndSelects() [][]byte {
	var calls [][]byte
	for _, j := range s.all() {
		c := []byte{0, 0}
		for _, k := range s.all() {
			if k == j {
				c = append(c, seedOther(whereTypes[s.types[k]], j, j%2 == 1)...)
				continue
			}
			c = append(c, seedPool(3, false)...)
		}
		calls = append(calls, c)
	}
	c := []byte{0, 0, 7 | 8 | 3<<4, 0}
	calls = append(calls, slices.Concat(c, s.row(s.all()[1:], 3)))
	n := byte(len(s.types))
	calls = append(calls,
		[]byte{1 | 1<<1 | 3<<2, 0, 1 | 1<<4, 2 | 2<<4, 3 | 3<<4},
		[]byte{1 | 1<<1 | 1<<2, n - 1, n - 1 | 1<<4},
		[]byte{1 | 1<<1, n},
		[]byte{1 | 1<<1 | 1<<6, n | 2<<4},
	)
	return calls
}

// wheres of the shape: selects with a WHERE of one condition on each column, with each operator in
// turn and a value of the pool, of the edges, or NULL, as a literal and as an argument; a test for
// NULL of each column; and two conditions, one of them on x.
func (s columnsSeedShape) wheres() [][]byte {
	var calls [][]byte
	ops := 0
	for _, j := range s.all() {
		for k := range 2 {
			value := seedPool(k+j, k == 1)
			if j == s.key && k == 1 {
				value = seedEdge(1, true)
			}
			calls = append(calls, slices.Concat([]byte{1 | 1<<4, byte(j | k<<4), byte(ops % 8)}, value))
			ops++
		}
	}
	n := byte(len(s.types))
	calls = append(calls,
		[]byte{1 | 1<<1 | 1<<4, 0, byte(s.key), 8},
		[]byte{1 | 2<<4, n - 1, 9, byte(s.key), 6, 4 | 8},
		[]byte{1 | 2<<4, 0, 0, 0, n, 0, 0},
	)
	return calls
}

// long of the shape: inserts of text and blobs about the lowered length limit of 1001 bytes, as
// literals and as arguments: 800 bytes in each column of either type, which make a record that is
// too long where two columns hold them, and 1,000 bytes and 1,005 bytes, which is too long, in the
// first column of either type. The key is new where it is not long. It is nil for a shape without
// a column of either type.
func (s columnsSeedShape) long() [][]byte {
	if !slices.ContainsFunc(s.types, func(typ int) bool { return whereTypes[typ] == "text" || whereTypes[typ] == "blob" }) {
		return nil
	}
	var calls [][]byte
	for r, length := range []byte{160, 200, 201} {
		for a, arg := range []bool{false, true} {
			c := []byte{0, 0}
			long := false
			for _, j := range s.all() {
				typ := whereTypes[s.types[j]]
				switch {
				case (typ == "text" || typ == "blob") && (length == 160 || !long):
					c = append(c, 6|seedArg(arg)|byte(r)<<4, length)
					long = true
				case j == s.key:
					c = append(c, s.newKey(5+2*r+a, arg)...)
				default:
					c = append(c, seedPool((r+j)%4, arg)...)
				}
			}
			calls = append(calls, c)
		}
	}
	return append(calls, []byte{1})
}

// columnsShape of a table of [FuzzColumns]: the names of its columns, their types, whether each takes
// NULL, and the index of its key.
type columnsShape struct {
	names    []string
	types    []string
	nullable []bool
	key      int
}

// columnsInput is the bytes of the input of [FuzzColumns] that are not read yet.
type columnsInput []byte

// next byte, or 0 once the input is read.
func (in *columnsInput) next() byte {
	if len(*in) == 0 {
		return 0
	}
	b := (*in)[0]
	*in = (*in)[1:]
	return b
}

// take n bytes, or what is left.
func (in *columnsInput) take(n int) []byte {
	n = min(n, len(*in))
	b := (*in)[:n]
	*in = (*in)[n:]
	return b
}

// columnsCalls of [FuzzColumns], from the bytes data: a create table of the table t and up to 16
// inserts and selects on it, then a select of every column. Once data is read, every byte is 0.
//
// The create table reads a byte for the count of columns, two to four, a byte for the index of the
// key, a byte for each column, and a byte for the order of the options. The byte of a column
// chooses its type, one of the five, by its lowest three bits modulo 5. With 8, a column that is not
// the key takes NULL, and the key says NOT NULL after PRIMARY KEY; with 16, the key says it before.
// A column that is not the key and does not take NULL says NOT NULL. The columns are c0 to c3.
//
// After that each call reads an op byte, which is an insert if it is even and a select if not, while
// the input has bytes. Bit 6 of the op names the table T.
//
// An insert reads, in this order: a byte whose bits 2j and 2j+1 spell the name of column j in its
// list (see [columnsName]); the bytes of its list, which bits 1 and 2 of the op choose: none for no
// list, none for the columns in the order of the table, a byte for each column but the first to
// shuffle the columns, or a byte whose bits 0 to 3 keep the columns and then a byte for each kept
// column but the first to shuffle them, where a byte that keeps none keeps the column of its bits 4
// to 7; the byte of its fault, if the fault reads one; and its values. Bits 3 to 5 of the op choose
// the fault: for 1 a value more, for the column of a byte; for 2 a value fewer, where there are two
// or more; for 3 the name at the position of a byte replaced by x, which the table does not have;
// for 4 the name at the position of a byte twice, which s4ql does not support; and no fault for 0
// and for 5 to 7. Faults 3 and 4 need a list, and read no byte without one. Each value is as
// [columnsValue] gives it for the type of its column, or of the key for x.
//
// A select is of every column, or with bit 1 of the op of named columns, one to four by bits 2 and 3,
// each by a byte whose bits 0 to 3, modulo the count of columns and one, are c0 to c3 or x, and whose
// bits 4 and 5 spell it. Bits 4 and 5 of the op give the number of conditions of its WHERE, zero to
// three, each by a byte for the column as for a name of the list, a byte for one of the eight
// operators of slice 7 in one of its spellings, and for a comparison a value of the type of the column,
// or of the key for x.
func columnsCalls(data []byte) []stmttest.Call {
	in := columnsInput(data)
	n := 2 + int(in.next())%3
	s := columnsShape{key: int(in.next()) % n}
	var defs []string
	for j := range n {
		b := in.next()
		name, typ := fmt.Sprint("c", j), whereTypes[int(b&7)%len(whereTypes)]
		def := name + " " + typ
		nullable := false
		switch {
		case j == s.key && b&16 != 0:
			def += " not null primary key"
		case j == s.key && b&8 != 0:
			def += " primary key not null"
		case j == s.key:
			def += " primary key"
		case b&8 != 0:
			nullable = true
		default:
			def += " not null"
		}
		s.names, s.types, s.nullable = append(s.names, name), append(s.types, typ), append(s.nullable, nullable)
		defs = append(defs, def)
	}
	options := " strict, without rowid"
	if in.next()%2 == 1 {
		options = " without rowid, strict"
	}
	calls := []stmttest.Call{{Src: "create table t (" + strings.Join(defs, ", ") + ")" + options, Supported: true}}
	for range 16 {
		if len(in) == 0 {
			break
		}
		op := in.next()
		if op%2 == 0 {
			calls = append(calls, columnsInsert(s, &in, op))
			continue
		}
		calls = append(calls, columnsSelect(s, &in, op))
	}
	return append(calls, stmttest.Call{Src: "select * from t", Query: true, Supported: true})
}

// columnsName of the column j of the shape s, or x for j outside it, spelt by the bits 0 and 1 of
// spelling: in upper case for 1, in double quotes for 2.
func columnsName(s columnsShape, j int, spelling byte) string {
	name := "x"
	if j < len(s.names) {
		name = s.names[j]
	}
	if spelling&1 != 0 {
		name = strings.ToUpper(name)
	}
	if spelling&2 != 0 {
		name = `"` + name + `"`
	}
	return name
}

// columnsInsert into the table of the shape s, by the op byte and the bytes of in, as [columnsCalls]
// says.
func columnsInsert(s columnsShape, in *columnsInput, op byte) stmttest.Call {
	n := len(s.names)
	spelling := in.next()
	shuffle := func(list []int) {
		for j := len(list) - 1; j > 0; j-- {
			k := int(in.next()) % (j + 1)
			list[j], list[k] = list[k], list[j]
		}
	}
	list := make([]int, n)
	for j := range list {
		list[j] = j
	}
	mode := op >> 1 % 4
	switch mode {
	case 2:
		shuffle(list)
	case 3:
		mask := in.next()
		list = slices.DeleteFunc(list, func(j int) bool { return mask&(1<<j) == 0 })
		if len(list) == 0 {
			list = []int{int(mask>>4) % n}
		}
		shuffle(list)
	}
	c := stmttest.Call{Supported: true}
	columns := slices.Clone(list)
	switch op >> 3 % 8 {
	case 1:
		columns = append(columns, int(in.next())%n)
	case 2:
		if len(columns) > 1 {
			columns = columns[:len(columns)-1]
		}
	case 3:
		if mode > 0 {
			at := int(in.next()) % len(list)
			list[at], columns[at] = n, n
		}
	case 4:
		if mode > 0 {
			list = append(list, list[int(in.next())%len(list)])
			columns = append(columns, list[len(list)-1])
			// A column that the list names twice is not supported.
			c.Supported = false
		}
	}
	var b strings.Builder
	b.WriteString("insert into t")
	if op>>6&1 != 0 {
		b.Reset()
		b.WriteString("insert into T")
	}
	if mode > 0 {
		var names []string
		for _, j := range list {
			names = append(names, columnsName(s, j, spelling>>(2*(j%4))&3))
		}
		b.WriteString(" (" + strings.Join(names, ", ") + ")")
	}
	var values []string
	for _, j := range columns {
		typ := s.types[s.key]
		if j < n {
			typ = s.types[j]
		}
		text, arg, isArg := columnsValue(typ, in)
		values = append(values, text)
		if isArg {
			c.Args = append(c.Args, arg)
		}
	}
	b.WriteString(" values (" + strings.Join(values, ", ") + ")")
	c.Src = b.String()
	return c
}

// columnsSelect from the table of the shape s, by the op byte and the bytes of in, as [columnsCalls]
// says.
func columnsSelect(s columnsShape, in *columnsInput, op byte) stmttest.Call {
	n := len(s.names)
	c := stmttest.Call{Query: true, Supported: true}
	list := "*"
	if op>>1&1 != 0 {
		var names []string
		for range 1 + int(op>>2%4) {
			b := in.next()
			names = append(names, columnsName(s, int(b&15)%(n+1), b>>4&3))
		}
		list = strings.Join(names, ", ")
	}
	var conditions []string
	ops := []string{"=", "==", "!=", "<>", "<", "<=", ">", ">=", "is null", "is not null"}
	for range int(op >> 4 % 4) {
		b := in.next()
		j := int(b&15) % (n + 1)
		operator := ops[int(in.next())%len(ops)]
		condition := columnsName(s, j, b>>4&3) + " " + operator
		if !strings.HasPrefix(operator, "is") {
			typ := s.types[s.key]
			if j < n {
				typ = s.types[j]
			}
			text, arg, isArg := columnsValue(typ, in)
			condition += " " + text
			if isArg {
				c.Args = append(c.Args, arg)
			}
		}
		conditions = append(conditions, condition)
	}
	c.Src = "select " + list + " from t"
	if op>>6&1 != 0 {
		c.Src = "select " + list + " from T"
	}
	if len(conditions) > 0 {
		c.Src += " where " + strings.Join(conditions, " and ")
	}
	return c
}

// columnsPools are values of each type of [whereTypes] that [columnsValue] draws often, so that keys
// meet.
var columnsPools = map[string][]any{
	"int":     {int64(0), int64(1), int64(-1), int64(2)},
	"integer": {int64(0), int64(1), int64(-1), int64(2)},
	"real":    {0.0, 1.5, -2.0, 0.5},
	"text":    {"", "a", "b", "A"},
	"blob":    {[]byte{}, []byte{0}, []byte{1}, []byte("a")},
}

// columnsValue for a column of the type typ, from the bytes of in: its text in the statement, and
// its argument if the text is a ?. A byte chooses, by its value modulo 8: for 0 and 1 a value of
// typ from [columnsPools], by the bits 4 and 5; for 2 and 3 a value of typ from the bytes that
// follow, as [whereValue] gives it; for 4 NULL; for 5 a value of another type from the bytes that
// follow; for 6 text or a blob of five times as many bytes as the next byte says, for a column of
// either, and a value from the bytes otherwise; and for 7 a value at an edge of typ from [whereRows]
// by the bits 4 to 7, or an argument that no column holds from [stmttest.Unsupported] where those
// bits are 3 and 8 is set. With 8 the value is an argument, and a literal otherwise.
func columnsValue(typ string, in *columnsInput) (text string, arg any, isArg bool) {
	b := in.next()
	var v any
	switch b % 8 {
	case 0, 1:
		pool := columnsPools[typ]
		v = pool[int(b>>4)%len(pool)]
	case 2, 3:
		v = whereValue(typ, in.take)
	case 4:
		v = nil
	case 5:
		others := slices.DeleteFunc(slices.Clone(whereTypes), func(o string) bool {
			return o == typ || strings.HasPrefix(o, "int") && strings.HasPrefix(typ, "int")
		})
		v = whereValue(others[int(b>>4)%len(others)], in.take)
	case 6:
		length := 5 * int(in.next())
		switch typ {
		case "text":
			v = strings.Repeat(string(rune('a'+b>>4)), length)
		case "blob":
			v = slices.Repeat([]byte{b >> 4}, length)
		default:
			v = whereValue(typ, in.take)
		}
	case 7:
		if b>>4 == 3 && b&8 != 0 {
			return "?", stmttest.Unsupported[int(in.next())%len(stmttest.Unsupported)], true
		}
		edges := whereRows[typ]
		v = edges[int(b>>4)%len(edges)]
		if f, ok := v.(float64); ok && f == 0 && b>>4%2 == 1 {
			// -0.0, which SQLite stores as 0.0.
			v = math.Copysign(0, -1)
		}
	}
	if b&8 != 0 {
		return "?", v, true
	}
	return whereLiteral(v), nil, false
}
