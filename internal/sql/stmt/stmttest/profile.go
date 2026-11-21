package stmttest

import (
	"hash/fnv"
	"math/bits"
)

// Profile of a [Sequence]: how often it writes each kind of statement, and the odds of each area
// of its generator, 0 for an area that is off. A profile weights one area over the statements that
// the area needs to mean something, and [Mixed] has every area on, since faults live where areas
// meet. A sequence has the profile of its seed, as [ProfileOf] gives it.
//
// An area that is off in a profile draws no random number in a sequence of the profile, so a change
// of its generator does not change such a sequence: the stream of such an area panics at a draw.
type Profile struct {
	Name string
	// slot of the profile among the [Slots] of the seeds.
	slot uint64
	// Weights of the kinds of statement, by [Kind]: a statement is of a kind as often as its weight
	// is of their sum.
	Weights [KindDropIndex + 1]int
	// Aim is the number of statements in four that are aimed at the tables as the sequence left them,
	// as [Sequence.aim] says, so that they meet a table that is there; 0 for a sequence that names its
	// tables at random, most of whose statements meet none.
	Aim int
	// Faults are the areas whose statements have their faults as often as the generator says for
	// each: a column that the table does not have, a value more or less, a value of another type, a
	// contradiction, and so on. The statements of the other areas have each of their faults a quarter
	// as often, as [Sequence.fault] says, so that the statements that a profile weighs meet tables
	// with rows.
	Faults []string
	// Columns is the least number of columns of the shape of a table, or 0 for any number from one.
	// With 2, each table has a column beside its key, for an index that does not hold the key first.
	Columns int
	// Others, if not 0, makes one text in Others one of [Others], in place of a statement.
	Others int
	// Where is the number of selects in four that have a WHERE.
	Where int
	// Indexes is 0 for a sequence without indexes, whose weights of a create index and a drop index
	// are 0; 1 for one or two names of indexes, and one WHERE in two of a table with an index that
	// pins leading columns of the index; and 2 for two or three names, three such WHERE in four, more
	// create indexes of every column but the key, and more WHERE that pin every column of an index,
	// and the key after them.
	Indexes int
	// Exists is [Generator.Exists]: one create and drop in Exists says IF NOT EXISTS or IF EXISTS.
	Exists int
	// Defaults is 0 for shapes without a DEFAULT; 1 for shapes of which a third have none, a third one
	// on every column, and a third one on each column one time in two; and 2 for shapes that all have
	// one, on every column or on each one time in two.
	Defaults int
	// ValueFirst is [Generator.ValueFirst]: one comparison in ValueFirst has its value on the left.
	ValueFirst int
	// Parameters is [Generator.Parameters]: one value in Parameters is a plain ?.
	Parameters int
	// Around makes one call in Around[0] of a statement with a parameter, and one in Around[1] of a
	// statement without one, have arguments around those that s4ql supports, as [Sequence.Next] says;
	// 0 for none.
	Around [2]int
	// Mistyped, if not 0, makes one insert in Mistyped give an integer of a column of INT or INTEGER,
	// if it has one, as the real of the same value, which s4ql refuses, as it refuses every value of
	// another type than its column. SQLite converts the real back to the integer where a real holds
	// the integer exactly, as it does for the small integers that a sequence mostly gives; for an
	// integer beyond 2^53 the real is a neighbour of it, and SQLite refuses ±2^63, the real of the
	// limits of 64 bits. The values that the conditions of a WHERE draw from keep the integer.
	Mistyped int
	// Transactions is the odds of a transaction, which a sequence does not write itself: what runs
	// the calls of a sequence in transactions begins one at one call in Transactions of those outside
	// a transaction.
	Transactions int
	// Order is the number of selects in four that have an ORDER BY of slice 13, as
	// [Sequence.order] draws it, and Limit, if not 0, makes one select in Limit have a LIMIT; the
	// area of the ORDER BY and the LIMIT is off with both 0.
	Order, Limit int
	// Keys, if not 0, makes one insert in Keys give the key a value of its statement, as [keyOfStep]
	// gives it, so that the tables of the sequence hold more rows; with 1, every insert does, and an
	// insert meets a key that exists only where another value of the sequence is the same.
	Keys int
	// Shared, if not 0, makes one create index in Shared of a table of which the sequence keeps an
	// index hold the first column of such an index and then another column, so that two indexes of
	// a table begin alike, as goqite's two do.
	Shared int
	// Aimed, if not 0, makes one select in Aimed of a table of which the sequence keeps an index have
	// a WHERE and an ORDER BY that are drawn together for the key of one of its indexes, as
	// [Sequence.aimedWhere] draws them: goqite's receive, or a pin of one column of the key and an
	// ORDER BY of the rest.
	Aimed int
	// Ties, if not 0, makes one value in Ties of an insert, for a column that is not the key and gets
	// no NULL, one of two values of its type, as [tieValue] gives them, so that rows tie on a column
	// of an index and differ in the next, where the order of a key and of an ORDER BY can part.
	Ties int
}

// Slots is the number of seeds after which the profiles repeat: the profile of a seed is the one at
// the slot of the seed modulo Slots. It is more than the profiles, so that a profile that is added
// takes a slot of its own, and the seeds of the others keep their profiles. A seed of a slot that
// no profile has is of [Mixed], until a profile takes the slot.
const Slots = 32

// The profiles. See [Profiles].
var (
	// Mixed has every area on, with the odds of each as a statement of several features meets them.
	Mixed = &Profile{Name: "mixed", Faults: allFaults, Weights: weights(4, 10, 5, 1, 3, 2, 3, 1), Others: 12, Where: 2, Indexes: 1, Exists: 3, Defaults: 1,
		ValueFirst: 3, Parameters: 3, Around: [2]int{4, 20}, Transactions: 4, Order: 1, Limit: 4}
	// Tables weighs the tables themselves, of slices 1, 4, 5, and 6: their columns, types, and keys,
	// the columns that take NULL, the lists and the faults of an insert, whole reals for a column of
	// integers, and a select of all rows.
	Tables = &Profile{Name: "tables", Faults: []string{areaTables, areaInserts}, Aim: 3, Weights: weights(2, 14, 6, 1, 0, 0, 0, 0), Parameters: 3, Mistyped: 8, Transactions: 4}
	// Where weighs the select with WHERE of slice 7, on keys and other columns.
	Where = &Profile{Name: "where", Faults: []string{areaSelects}, Aim: 3, Weights: weights(2, 9, 12, 1, 0, 0, 0, 0), Where: 3, Parameters: 3, Transactions: 4}
	// Changes weighs the update and the delete of slice 8.
	Changes = &Profile{Name: "changes", Faults: []string{areaChanges}, Aim: 3, Weights: weights(2, 12, 3, 1, 9, 3, 0, 0), Where: 2, Parameters: 3, Transactions: 4}
	// Indexes weighs the create index and the drop index of slice 9, and the WHERE that an index
	// serves.
	Indexes = &Profile{Name: "indexes", Faults: []string{areaIndexes}, Aim: 3, Weights: weights(2, 8, 8, 1, 3, 2, 4, 2), Where: 3, Indexes: 2, Columns: 2, Parameters: 3, Transactions: 4}
	// Exists weighs the IF NOT EXISTS and IF EXISTS of slice 10, on tables and indexes that are there
	// and that are not.
	Exists = &Profile{Name: "exists", Faults: []string{areaTables, areaIndexes}, Aim: 2, Weights: weights(6, 5, 2, 3, 1, 1, 7, 3), Where: 2, Indexes: 1, Exists: 2, Parameters: 3, Transactions: 4}
	// Defaults weighs the DEFAULT of slice 11, and the inserts that leave a column out.
	Defaults = &Profile{Name: "defaults", Aim: 3, Weights: weights(4, 15, 6, 1, 1, 0, 0, 0), Where: 1, Defaults: 2, Parameters: 3, Transactions: 4}
	// ValueFirst weighs the value on the left of a comparison of slice 12, in the WHERE of a select,
	// an update, and a delete, on keys and on indexes.
	ValueFirst = &Profile{Name: "valuefirst", Faults: []string{areaSelects, areaChanges}, Aim: 3, Weights: weights(2, 8, 8, 1, 7, 3, 3, 1), Where: 4, Indexes: 2, Columns: 2, ValueFirst: 2, Parameters: 3, Around: [2]int{3, 12}, Transactions: 4}
	// Transactions weighs the statements that write and those that read what was written, on a
	// sequence whose test begins transactions more often.
	Transactions = &Profile{Name: "transactions", Aim: 3, Weights: weights(2, 10, 6, 1, 4, 3, 1, 1), Where: 2, Indexes: 1, Parameters: 3, Transactions: 2}
	// Arguments weighs the parameters of slice 2, the arguments around those that s4ql supports, and
	// the texts of [Others]: the errors and the refusals.
	Arguments = &Profile{Name: "arguments", Faults: allFaults, Aim: 3, Weights: weights(2, 10, 6, 1, 3, 2, 0, 0), Others: 4, Where: 3, Parameters: 1, Around: [2]int{2, 6}, Transactions: 4}
	// Order weighs the ORDER BY and the LIMIT of slice 13: of every type of column, ascending and
	// descending, with NULL and with rows that tie, with WHERE and without, by an index that delivers
	// the order and beside one that only narrows a range, and the LIMIT as a literal and as a ?, also
	// with an argument that s4ql refuses.
	Order = &Profile{Name: "order", Faults: []string{areaOrder}, Aim: 3, Weights: weights(2, 14, 9, 1, 1, 1, 6, 1), Where: 2, Indexes: 2, Columns: 3,
		Parameters: 3, Transactions: 4, Order: 3, Limit: 2, Keys: 1, Shared: 2, Aimed: 3, Ties: 2}
)

// allFaults are the areas whose statements have faults, for [Profile.Faults].
var allFaults = []string{areaTables, areaInserts, areaSelects, areaChanges, areaIndexes, areaOrder}

// Profiles, each at the slot of its place in the list. A profile is added at the end, and none is
// taken out or moved, since the seeds of each profile are those of its slot: see [Profile.Seed].
var Profiles = func() []*Profile {
	ps := []*Profile{Mixed, Tables, Where, Changes, Indexes, Exists, Defaults, ValueFirst, Transactions, Arguments, Order}
	if len(ps) > Slots {
		panic("stmttest: more profiles than slots")
	}
	for i, p := range ps {
		p.slot = uint64(i)
	}
	return ps
}()

// weights of the kinds of statement of a [Profile]: create table, insert, select, drop table,
// update, delete, create index, and drop index.
func weights(createTable, insert, selects, dropTable, update, del, createIndex, dropIndex int) [KindDropIndex + 1]int {
	return [KindDropIndex + 1]int{KindCreateTable: createTable, KindInsert: insert, KindSelect: selects, KindDropTable: dropTable,
		KindUpdate: update, KindDelete: del, KindCreateIndex: createIndex, KindDropIndex: dropIndex}
}

// ProfileOf the seed: the profile at the slot of the seed modulo [Slots], and [Mixed] for a slot
// that no profile has yet.
func ProfileOf(seed uint64) *Profile {
	if slot := seed % Slots; slot < uint64(len(Profiles)) {
		return Profiles[slot]
	}
	return Mixed
}

// Seed number k, from 0, of the profile: the seeds of a profile are those of its slot, so a test
// that runs the first seeds of each profile runs the same seeds of each when a profile is added.
func (p *Profile) Seed(k uint64) uint64 {
	return k*Slots + p.slot
}

// The areas of the generator of a [Sequence]. Each draws its random numbers from a stream of its
// own, which is seeded from the seed of the sequence, the name of the area, and what the numbers are
// for: the number of the statement in the sequence, or the name of the table whose shape they draw.
// So a draw more or less in one area, or in one statement, moves no number of another area or of
// another statement.
const (
	// areaNames draws the names of the tables and the columns of the sequence, and their spellings.
	areaNames = "names"
	// areaPlan draws which statement comes next, and the name of its table or its index.
	areaPlan = "plan"
	// areaTables draws the shape of a table, and the columns of a create table that differ from it.
	areaTables = "tables"
	// areaInserts draws the list and the values of an insert, and its faults.
	areaInserts = "inserts"
	// areaSelects draws the select list and the WHERE of a select.
	areaSelects = "selects"
	// areaChanges draws the assignments of an update, and the WHERE of an update and a delete.
	areaChanges = "changes"
	// areaIndexes draws the names of the indexes, the columns of a create index, and the WHERE that
	// an index serves, of a select, an update, and a delete.
	areaIndexes = "indexes"
	// areaExists draws whether a create or a drop says IF NOT EXISTS or IF EXISTS.
	areaExists = "exists"
	// areaDefaults draws the defaults of a shape, those of a create table that differ from it, and
	// whether an insert leaves out a column with a default.
	areaDefaults = "defaults"
	// areaValueFirst draws whether a comparison has its value on the left.
	areaValueFirst = "valuefirst"
	// areaArguments draws whether a value is a parameter, and the arguments around those of a call.
	areaArguments = "arguments"
	// areaOrder draws the ORDER BY and the LIMIT of a select, and an argument of the LIMIT that s4ql
	// refuses.
	areaOrder = "order"
	// areaOthers draws the texts of [Others].
	areaOthers = "others"
	// areaText draws how a statement is written: its gaps, the case of its keywords, the spelling of
	// its literals and operators, and its semicolons.
	areaText = "text"
)

// on reports whether the area is on in the profile: whether a sequence of the profile may draw
// from the streams of the area.
func (p *Profile) on(area string) bool {
	changes := p.Weights[KindUpdate]+p.Weights[KindDelete] > 0
	switch area {
	case areaSelects:
		return p.Weights[KindSelect] > 0
	case areaChanges:
		return changes
	case areaIndexes:
		return p.Indexes > 0 || p.Weights[KindCreateIndex] > 0
	case areaExists:
		return p.Exists > 0
	case areaDefaults:
		return p.Defaults > 0
	case areaValueFirst:
		return p.ValueFirst > 0
	case areaArguments:
		return p.Parameters > 0 || p.Around != [2]int{}
	case areaOthers:
		return p.Others > 0
	case areaOrder:
		return p.Order > 0 || p.Limit > 0
	}
	return true
}

// mix the bits of x, as SplitMix64 does, so that seeds that differ in a bit give streams that have
// nothing in common.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

// keyOf the name of an area and what its numbers are for, such as the name of a table.
func keyOf(area, what string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(area))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(what))
	return h.Sum64()
}

// pcg is a generator of random numbers whose numbers are a function of its two seeds alone: the
// PCG of math/rand/v2, with the numbers below a bound as its Rand draws them on a machine of 64
// bits, written out here so that a stream draws nothing from the machine and gives the same numbers
// wherever it runs.
type pcg struct {
	hi, lo uint64
}

func (p *pcg) Uint64() uint64 {
	// The state times a multiplier of 128 bits, plus an increment.
	hi, lo := bits.Mul64(p.lo, 4865540595714422341)
	hi += p.hi*4865540595714422341 + p.lo*2549297995355413924
	lo, carry := bits.Add64(lo, 1442695040888963407, 0)
	hi, _ = bits.Add64(hi, 6364136223846793005, carry)
	p.hi, p.lo = hi, lo
	// The output, by two xorshifts and two multiplications.
	hi ^= hi >> 32
	hi *= 0xda942042e4dd58b5
	hi ^= hi >> 48
	hi *= lo | 1
	return hi
}

// IntN is a number in [0, n), each as likely as another. It panics if n is not above 0.
func (p *pcg) IntN(n int) int {
	if n <= 0 {
		panic("stmttest: a number below a bound that is not above 0")
	}
	bound := uint64(n)
	if bound&(bound-1) == 0 {
		return int(p.Uint64() & (bound - 1))
	}
	// The high half of a number times the bound, drawn again where the low half is among the few
	// values that would make some numbers more likely than others.
	hi, lo := bits.Mul64(p.Uint64(), bound)
	if lo < bound {
		for threshold := -bound % bound; lo < threshold; {
			hi, lo = bits.Mul64(p.Uint64(), bound)
		}
	}
	return int(hi)
}

// newStream of the seed for the area and what its numbers are for: the same numbers for the same
// three, whatever else a sequence draws.
func newStream(seed uint64, area, what string) *pcg {
	return &pcg{hi: mix(seed), lo: mix(keyOf(area, what))}
}

// stepStream is the stream of an area that starts anew at each statement of a sequence: its
// numbers for the statement number step are those of the seed, the area, and step, whatever the
// statements before drew.
type stepStream struct {
	seed, key uint64
	// step is the number of the statement of the sequence, and at one more than the step that the
	// generator is seeded for, or 0 before the first number.
	step *uint64
	at   uint64
	pcg  pcg
}

func newStepStream(seed uint64, area string, step *uint64) *stepStream {
	return &stepStream{seed: mix(seed), key: keyOf(area, "statement"), step: step}
}

// sync seeds the stream for the statement of now, if it is another than that of the last number.
func (s *stepStream) sync() {
	if s.at != *s.step+1 {
		s.at = *s.step + 1
		s.pcg = pcg{hi: s.seed, lo: mix(s.key ^ mix(*s.step))}
	}
}

func (s *stepStream) IntN(n int) int {
	s.sync()
	return s.pcg.IntN(n)
}

func (s *stepStream) Uint64() uint64 {
	s.sync()
	return s.pcg.Uint64()
}

// offStream is the stream of an area that is off in the profile of a sequence: a draw from it
// panics, since such an area draws nothing.
type offStream struct {
	area, profile string
}

func (s offStream) IntN(int) int {
	panic("stmttest: a sequence of the profile " + s.profile + " draws from the area " + s.area + ", which the profile has off")
}

func (s offStream) Uint64() uint64 {
	panic("stmttest: a sequence of the profile " + s.profile + " draws from the area " + s.area + ", which the profile has off")
}
