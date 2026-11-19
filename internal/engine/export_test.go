package engine

// Plant the bug with the given name in o, which is a field of [bugs]. It panics for a name that is
// none.
func (o *Options) Plant(name string) {
	set, ok := plantable[name]
	if !ok {
		panic("engine: no planted bug is named " + name)
	}
	set(&o.bugs)
}

// plantable bugs, by the names of the fields of [bugs].
var plantable = map[string]func(b *bugs){
	"skipUnique":    func(b *bugs) { b.skipUnique = true },
	"ackUnknown":    func(b *bugs) { b.ackUnknown = true },
	"checkOutside":  func(b *bugs) { b.checkOutside = true },
	"keepOpen":      func(b *bugs) { b.keepOpen = true },
	"keepRead":      func(b *bugs) { b.keepRead = true },
	"annotateNext":  func(b *bugs) { b.annotateNext = true },
	"keepFailed":    func(b *bugs) { b.keepFailed = true },
	"readNewest":    func(b *bugs) { b.readNewest = true },
	"rollbackKeeps": func(b *bugs) { b.rollbackKeeps = true },
}

// The names of the event and of its attributes, for the tests of the telemetry.
const (
	EventStatement   = eventStatement
	AttrKind         = attrKind
	AttrTable        = attrTable
	AttrSQL          = attrSQL
	AttrRows         = attrRows
	AttrPosition     = attrPosition
	AttrParent       = attrParent
	AttrOutcome      = attrOutcome
	AttrError        = attrError
	AttrCode         = attrCode
	AttrExtendedCode = attrExtendedCode
	AttrDuration     = attrDuration
)

// FromCore is [fromCore].
var FromCore = fromCore
