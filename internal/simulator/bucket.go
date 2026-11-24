//go:build s4ql_browser

package simulator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
)

// StoredBucket is what a page keeps of a database between two runs, as S3 would keep it: the objects
// under the prefix of the database, the counter of the workload, and the time of the history. A run
// that is given one starts on a store that holds the objects, as an application that starts on a
// new machine finds its bucket.
type StoredBucket struct {
	// Version of the storage that wrote the objects, which must be [StorageVersion].
	Version string `json:"version"`
	// Objects under the prefix, in the order in which they were last written.
	Objects []StoredObject `json:"objects"`
	// Values is the counter of the workload: the number of values that the writes before took. The
	// first write of the run takes the next.
	Values int64 `json:"values"`
	// From is the time of the history when the run starts: how long the runs before it lasted, in
	// milliseconds. The wall clocks of the run's processes start there.
	From float64 `json:"from"`
}

// StoredObject is an object of a [StoredBucket]: its key, its content, and when it was last written,
// as S3 keeps the time of its last modification, in milliseconds of the history.
type StoredObject struct {
	Key     string  `json:"key"`
	Content []byte  `json:"content"`
	At      float64 `json:"at"`
}

// maxStoredObjects of a [StoredBucket], so that a run stays small enough for a page.
const maxStoredObjects = 50_000

// storageShape is the version of the shape of a [StoredBucket] itself. It goes up when the meaning
// of its fields changes. The log of a bucket of shape 2 can delete rows.
const storageShape = 2

// StorageVersion of the objects that this engine writes: the shape of a [StoredBucket], and a hash
// of a manifest and of a commit object as the storage formats write them, so that it changes with
// the versions and the encodings of the formats.
func StorageVersion() string {
	h := sha256.New()
	m := format.Manifest{Lease: &format.Lease{Holder: "holder", Duration: 30 * time.Second, Renewed: sim.Epoch(), Salt: 1}}
	b, err := m.Marshal()
	if err != nil {
		panic("simulator: the manifest of the version cannot be written: " + err.Error())
	}
	h.Write(b)
	var c bytes.Buffer
	w, err := format.NewCommitWriter(&c, format.EmptyLog, sim.Epoch())
	if err == nil {
		err = w.UpsertRow(tableName, []uint32{0}, []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(int64(1))}.Build()})
	}
	if err == nil {
		err = w.Annotate(s4qlpb.Annotation_builder{Statement: s4qlpb.Statement_builder{Sql: new(insertSQL)}.Build()}.Build())
	}
	if err == nil {
		_, err = w.Close()
	}
	if err != nil {
		panic("simulator: the commit object of the version cannot be written: " + err.Error())
	}
	h.Write(c.Bytes())
	return fmt.Sprintf("%v-%x", storageShape, h.Sum(nil)[:8])
}

// prefixPattern of the prefix of a database: short, and safe in a key of S3.
var prefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// seed of a run: the objects of a [StoredBucket], checked and read, with the commit objects before the
// manifest, so that the store never holds a manifest whose log it lacks.
type seed struct {
	objects []StoredObject
	// commits holds what each commit object holds, by its key.
	commits map[string]*CommitObject
	// manifest is what the manifest says, or nil if the bucket has none.
	manifest *ManifestObject
	// log is the commit objects of the log, from the first to the head.
	log    []*CommitObject
	values int64
	from   time.Duration
}

// parseBucket checks the stored bucket b of the database with the prefix, and reads it. It refuses
// a bucket that no run of this engine could have left: another version, a key outside the prefix,
// an object that cannot be read, a manifest whose log is not whole, a key in the log twice, or a key
// that the counter has not reached.
func parseBucket(b *StoredBucket, prefix string) (*seed, error) {
	if b == nil {
		return nil, nil
	}
	if v := StorageVersion(); b.Version != v {
		return nil, fmt.Errorf("bucket.version is %q, and this engine writes %q", b.Version, v)
	}
	switch {
	case len(b.Objects) > maxStoredObjects:
		return nil, fmt.Errorf("bucket.objects has %v objects, and may have at most %v", len(b.Objects), maxStoredObjects)
	case b.Values < 0 || b.Values > 1e10:
		return nil, fmt.Errorf("bucket.values is %v, and must be from 0 to 10000000000", b.Values)
	case !nonNegative(b.From) || b.From > 1e13:
		return nil, fmt.Errorf("bucket.from is %v, and must be from 0 to 10000000000000", b.From)
	}
	s := &seed{commits: map[string]*CommitObject{}, values: b.Values, from: duration(b.From)}
	var manifest *StoredObject
	seen := map[string]bool{}
	for i, o := range b.Objects {
		switch {
		case seen[o.Key]:
			return nil, fmt.Errorf("bucket.objects[%v]: the key %q is there twice", i, o.Key)
		case !finite(o.At):
			return nil, fmt.Errorf("bucket.objects[%v]: at is %v", i, o.At)
		case o.Key == prefix+"/manifest.json":
			manifest = &b.Objects[i]
		default:
			c, err := storedCommit(o, prefix)
			if err != nil {
				return nil, fmt.Errorf("bucket.objects[%v]: %w", i, err)
			}
			s.commits[o.Key] = c
			s.objects = append(s.objects, o)
		}
		seen[o.Key] = true
	}
	if manifest == nil {
		return s, nil
	}
	m, err := format.ParseManifest(manifest.Content)
	if err != nil {
		return nil, fmt.Errorf("the manifest cannot be read: %w", err)
	}
	s.manifest = &ManifestObject{Head: m.Head.String()}
	if l := m.Lease; l != nil {
		s.manifest.Lease = &Lease{Holder: l.Holder, Duration: millis(l.Duration), Renewed: millis(l.Renewed.Sub(sim.Epoch())), Salt: fmt.Sprintf("%016x", l.Salt)}
	}
	s.objects = append(s.objects, *manifest)
	if err := s.readLog(); err != nil {
		return nil, err
	}
	return s, nil
}

// storedCommit reads the stored commit object o under the prefix.
func storedCommit(o StoredObject, prefix string) (*CommitObject, error) {
	rest, ok := strings.CutPrefix(o.Key, prefix+"/commits/")
	dir, file, _ := strings.Cut(rest, "/")
	if !ok || len(dir) != 2 || !strings.HasPrefix(file, dir) {
		return nil, fmt.Errorf("the key %q is neither the manifest nor a commit object of the prefix %q", o.Key, prefix)
	}
	name, err := format.ParseName(file)
	if err != nil {
		return nil, fmt.Errorf("the key %q: %w", o.Key, err)
	}
	c, err := readCommit(o.Content, name)
	if err != nil {
		return nil, fmt.Errorf("the commit object %v cannot be read: %w", short(name.String()), err)
	}
	return c, nil
}

// readLog of the seed from the head of its manifest, and check that it is a log that a run of this
// engine could have left: whole, with each key once, each key one that the counter has given, and
// each delete of a row that the log holds there.
func (s *seed) readLog() error {
	byName := map[string]*CommitObject{}
	for _, c := range s.commits {
		byName[c.Name] = c
	}
	empty := format.EmptyLog.String()
	for name := s.manifest.Head; name != empty; {
		c := byName[name]
		if c == nil {
			return fmt.Errorf("the log of the manifest has the commit object %v, which the bucket lacks", short(name))
		}
		if len(s.log) > len(byName) {
			return errors.New("the log of the manifest is a loop")
		}
		s.log = append(s.log, c)
		name = c.Parent
	}
	for i, j := 0, len(s.log)-1; i < j; i, j = i+1, j-1 {
		s.log[i], s.log[j] = s.log[j], s.log[i]
	}
	inLog, rows := map[int64]bool{}, map[int64]bool{}
	for _, c := range s.log {
		for _, k := range c.Keys {
			switch {
			case inLog[k]:
				return fmt.Errorf("the log holds the key %v twice", k)
			case !given(k, s.values):
				return fmt.Errorf("the log holds the key %v, which the counter of %v values has not given", k, s.values)
			}
			inLog[k], rows[k] = true, true
		}
		for _, k := range c.Deletes {
			if !rows[k] {
				return fmt.Errorf("the log deletes the key %v, which it does not hold there", k)
			}
			delete(rows, k)
		}
	}
	return nil
}

// seedWorkers write the objects of a seed into the store side by side.
const seedWorkers = 32

// seedStore puts the objects of the seed into the store before any instance starts, through a
// client of its own, and then tells the application, the fold, and the purposes of the requests what
// the store holds. Its event "seeded" has in by the time of the history at the time 0 of the run, which
// the wall clocks of the processes read then, so that the history goes on from the bucket's time
// when the instances start. Each object is written until the store holds it, up to [seedAttempts]
// times, so a fault of the store delays the seed and does not change it. If the store does not take
// an object, as under faults that make every write fail, the run is refused, and seedStore returns
// false. The events of its requests are not in the trace.
func (r *run) seedStore(ctx context.Context) bool {
	s := r.req.seed
	if s == nil {
		return true
	}
	r.mu.Lock()
	r.values = s.values
	r.mu.Unlock()
	c := r.store.Client("seed", nil)
	put := func(ctx context.Context, o StoredObject) {
		for range seedAttempts {
			if _, _, ok := r.store.Object(o.Key); ok {
				return
			}
			ctx, cancel := r.world.WithTimeout(ctx, 10*time.Second)
			_, _ = c.Create(ctx, o.Key, bytes.NewReader(o.Content), nil)
			cancel()
		}
		if _, _, ok := r.store.Object(o.Key); !ok {
			r.mu.Lock()
			r.refusal = fmt.Sprintf("the store did not take %v of the bucket in %v attempts", o.Key, seedAttempts)
			r.mu.Unlock()
		}
	}
	// The commit objects first, side by side, and the manifest once the store holds every one of them.
	commits := s.objects
	if s.manifest != nil {
		commits = commits[:len(commits)-1]
	}
	next := 0
	var workers []interface{ Wait(context.Context) error }
	for w := range min(seedWorkers, len(commits)) {
		workers = append(workers, r.world.Go(ctx, fmt.Sprint("seed ", w), func(ctx context.Context) {
			for {
				r.mu.Lock()
				i := next
				next++
				r.mu.Unlock()
				if i >= len(commits) {
					return
				}
				put(ctx, commits[i])
			}
		}))
	}
	for _, w := range workers {
		_ = w.Wait(ctx)
	}
	if r.refused() {
		return false
	}
	e := Event{Kind: "store", What: "seeded", Text: fmt.Sprintf("the store holds %v objects from before", len(s.objects))}
	if s.manifest != nil {
		key := s.objects[len(s.objects)-1].Key
		put(ctx, s.objects[len(s.objects)-1])
		if r.refused() {
			return false
		}
		_, version, _ := r.store.Object(key)
		e.Version = string(version)
		r.mu.Lock()
		r.manifests[version] = s.manifest
		r.mu.Unlock()
		r.keys.stored(s.log)
	}
	// The history goes on from where the bucket left it, from the time at which the instances start,
	// and never goes back before its start.
	offset := max(0, s.from-r.world.Monotonic())
	e.By = millis(offset)
	r.mu.Lock()
	r.offset = offset
	r.mu.Unlock()
	if r.rec.fold != nil {
		r.rec.fold.pending = s
	}
	r.rec.add(e)
	return true
}

// seedAttempts is the number of times that the seed writes an object before it gives up.
const seedAttempts = 100

// short form of the name of a commit object.
func short(name string) string {
	return name[:min(len(name), 8)]
}

// refused reports whether the run is refused.
func (r *run) refused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusal != ""
}

// changed records the object at key as the store holds it now, which a write has just put there,
// for the next answer of a session.
func (r *run) changed(key string, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, StoredObject{Key: key, Content: content, At: millis(r.offset + r.world.Monotonic())})
}

// takeChanges since the last take, and the counter of the workload now.
func (r *run) takeChanges() ([]StoredObject, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changes := r.changes
	r.changes = nil
	return changes, r.values
}

// given reports whether the key is the barcode of a value from 1 to values, by undoing [barcode].
func given(key, values int64) bool {
	const half = 100_000
	x := key / 10 % (half * half)
	hi, lo := x/half, x%half
	for round := int64(3); round >= 0; round-- {
		hi, lo = (lo-(hi*hi*31_337+hi*7_919+round*104_729+12_345)%half+half)%half, hi
	}
	n := hi*half + lo
	if n == 0 {
		n = half * half
	}
	return n <= values && barcode(n) == key
}
