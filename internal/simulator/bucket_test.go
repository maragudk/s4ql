//go:build s4ql_browser

package simulator_test

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// kept bucket of a session, as a page keeps it: the objects that the changes of its answers wrote,
// each as its last write left it, the counter, and the time of the last answer.
type kept struct {
	objects map[string]simulator.StoredObject
	values  int64
	t       float64
	version string
	from    float64
}

func newKept() *kept {
	return &kept{objects: map[string]simulator.StoredObject{}}
}

// keep the changes of the answer a.
func (k *kept) keep(a simulator.Live) {
	for _, c := range a.Changes {
		k.objects[c.Key] = c
	}
	k.values, k.t = a.Values, a.T
	if a.Version != "" {
		k.version = a.Version
	}
}

// bucket that a page would store, for a session that started from the time from of the history.
func (k *kept) bucket() simulator.StoredBucket {
	objects := slices.SortedFunc(maps.Values(k.objects), func(a, b simulator.StoredObject) int {
		return cmp.Or(cmp.Compare(a.At, b.At), strings.Compare(a.Key, b.Key))
	})
	return simulator.StoredBucket{Version: k.version, Objects: objects, Values: k.values, From: k.from + k.t}
}

// session of a page that keeps the bucket: every answer goes into kept.
type keeping struct {
	*live
	kept *kept
}

// startKeeping a session with the settings, given as the members of a JSON object, on the bucket
// b, or none if nil.
func startKeeping(t *testing.T, settings string, b *simulator.StoredBucket) *keeping {
	t.Helper()
	k := &keeping{kept: newKept()}
	body := "{" + settings + "}"
	if b != nil {
		data, err := json.Marshal(b)
		is.NotError(t, err)
		body = "{" + settings + `, "bucket": ` + string(data) + "}"
		k.kept.from = b.From
		for _, o := range b.Objects {
			k.kept.objects[o.Key] = o
		}
		k.kept.values = b.Values
	}
	k.live = startLive(t, body)
	k.kept.keep(k.last)
	return k
}

func (k *keeping) advance(ms float64) simulator.Live {
	k.t.Helper()
	a := k.live.advance(ms)
	k.kept.keep(a)
	return a
}

func (k *keeping) act(action string) simulator.Live {
	k.t.Helper()
	a := k.do("act", action)
	k.kept.keep(a)
	return a
}

// end the session, and return its last answer, which the page that reloads never gets, so it is
// not kept.
func (k *keeping) end() simulator.Live {
	k.t.Helper()
	return k.do("end", "{}")
}

// keysOf the rows that the commit objects of the bucket insert.
func keysOf(t *testing.T, s simulator.State) map[int64]bool {
	t.Helper()
	keys := map[int64]bool{}
	for _, c := range s.Bucket.Commits {
		for _, key := range c.Keys {
			keys[key] = true
		}
	}
	return keys
}

// pageSettings are those of the simulator page: a starts alone, b stands by, and the client calls a.
const pageSettings = `"workload": {"callers": 0}, "client": {"instance": "a"}, "standby": true`

func TestEngine_bucket(t *testing.T) {
	t.Run("should continue a session on the bucket that it left, with its rows, new keys, and its deletes", func(t *testing.T) {
		first := startKeeping(t, `"seed": 7, `+pageSettings, nil)
		for range 20 {
			first.advance(15_000)
		}
		before := first.do("now", "{}").State
		is.True(t, len(before.Bucket.Log) > 20, "the first session commits")
		oldKeys := keysOf(t, *before)
		oldDeletes := map[int64]bool{}
		for _, c := range before.Bucket.Commits {
			for _, k := range c.Deletes {
				is.True(t, c.InLog, "a delete in the log")
				oldDeletes[k] = true
			}
		}
		is.True(t, len(oldDeletes) > 0, "the first session deletes")
		b := first.kept.bucket()

		second := startKeeping(t, `"seed": 8, `+pageSettings, &b)
		start := second.do("now", "{}").State
		is.Equal(t, len(before.Bucket.Log), len(start.Bucket.Log), "the second session starts with the log of the first")
		is.Equal(t, before.Bucket.Manifest.Head, start.Bucket.Manifest.Head)
		for range 20 {
			second.advance(15_000)
		}
		second.end()

		var opened, oldRows, newWrites int
		for _, e := range second.events {
			switch {
			case e.Kind == "process" && e.What == "opened" && e.Instance == "a":
				opened++
			case e.Kind == "call" && e.Op == "read" && e.What == "served" && len(e.Keys) == 1 && oldKeys[e.Keys[0]]:
				oldRows++
			case e.Kind == "call" && e.Op == "write" && e.What == "committed":
				newWrites++
				is.True(t, !oldKeys[e.Value], "a new write takes a key of its own")
			}
			// After the reload, the application knows the rows of the log, and not those that it
			// deleted: no call picks one of them.
			if e.Kind == "call" && e.What == "started" && e.Op != "write" {
				is.True(t, !oldDeletes[int64(e.Args[0].(float64))], fmt.Sprint("a ", e.Op, " of a row deleted before the reload: ", e.Args[0]))
			}
		}
		is.Equal(t, 1, opened)
		is.True(t, oldRows > 0, "reads give rows from before")
		is.True(t, newWrites > 0, "the second session commits")
		lease := find(simulator.Response{Events: second.events}, func(e simulator.Event) bool {
			return e.Kind == "core" && e.Event == "lease" && e.Attrs["lease.transition"] == "taken" && e.Attrs["outcome"] == "taken"
		})
		is.True(t, len(lease) > 0, "an instance takes the lease")
		is.Equal[any](t, before.Bucket.Manifest.Lease.Holder, lease[0].Attrs["lease.previous_holder"], "from the holder of the session before")
	})

	t.Run("should continue a session with crashes, faults, a hung and a disconnected network, and a lost answer just before the reload", func(t *testing.T) {
		settings := fmt.Sprintf(`"instances": 3, "faults": %v, "workload": {"think": 1000, "writes": 60}`, allFaults)
		for _, seed := range []int{1, 2, 3} {
			first := startKeeping(t, fmt.Sprintf(`"seed": %v, `, seed)+settings, nil)
			first.advance(20_000)
			first.act(`{"do": "crash", "instance": "b"}`)
			first.advance(10_000)
			first.act(`{"do": "hangNetwork", "instance": "a", "on": true}`)
			first.advance(40_000)
			first.act(`{"do": "hangNetwork", "instance": "a"}`)
			first.act(`{"do": "disconnectNetwork", "instance": "c", "on": true}`)
			first.advance(20_000)
			first.act(`{"do": "disconnectNetwork", "instance": "c"}`)
			first.act(`{"do": "restart", "instance": "b"}`)
			first.advance(30_000)
			first.act(`{"do": "loseAnswer", "request": "any"}`)
			// The answer of a write is lost, and its change still comes.
			lost := false
			for i := 0; i < 200 && !lost; i++ {
				a := first.advance(50)
				for _, e := range a.Events {
					if e.What == "answer_lost" {
						lost = true
					}
				}
			}
			is.True(t, lost, "an answer is lost")
			b := first.kept.bucket()

			second := startKeeping(t, fmt.Sprintf(`"seed": %v, `, seed+10)+settings, &b)
			second.advance(60_000)
			second.act(`{"do": "crash", "instance": "a"}`)
			second.advance(60_000)
			second.act(`{"do": "calm"}`)
			// A catch-up of the whole log under these faults can take minutes: a download that gets no
			// answer waits for the timeout of 5 min.
			for range 10 {
				second.advance(60_000)
			}
			second.end()
			committed := find(simulator.Response{Events: second.events}, func(e simulator.Event) bool {
				return e.Kind == "call" && e.Op == "write" && e.What == "committed"
			})
			is.True(t, len(committed) > 0, "the second session commits")
		}
	})

	t.Run("should report the change of every write with the answer that has its event, also when its answer is lost", func(t *testing.T) {
		k := startKeeping(t, `"seed": 3, `+pageSettings, nil)
		k.advance(60_000)
		k.act(`{"do": "loseAnswer", "request": "commit"}`)
		applied := map[int]bool{}
		lost := false
		for range 400 {
			a := k.advance(250)
			var keys []string
			for _, e := range a.Events {
				switch e.What {
				case "applied":
					applied[e.Request] = true
					keys = append(keys, e.Key)
				case "answer_lost":
					lost = lost || applied[e.Request]
				}
			}
			var changed []string
			for _, o := range a.Changes {
				changed = append(changed, o.Key)
			}
			is.EqualSlice(t, keys, changed)
		}
		is.True(t, lost, "the answer of an applied write was lost")
	})

	t.Run("should give the same events and hash for the same bucket, counter, time, and seed", func(t *testing.T) {
		first := startKeeping(t, `"seed": 5, `+pageSettings, nil)
		for range 8 {
			first.advance(15_000)
		}
		b := first.kept.bucket()
		var hashes []string
		var events [][]string
		for range 2 {
			k := startKeeping(t, `"seed": 6, `+pageSettings, &b)
			for range 8 {
				k.advance(15_000)
			}
			last := k.end()
			hashes = append(hashes, last.Hash)
			events = append(events, eventLines(t, append(k.events, last.Events...)))
		}
		is.Equal(t, hashes[0], hashes[1])
		is.EqualSlice(t, events[0], events[1])
		t.Logf("the hash of the seeded session: %v", hashes[0])
	})

	t.Run("should start the wall clocks and the commit objects at the time of the history", func(t *testing.T) {
		first := startKeeping(t, `"seed": 7, `+pageSettings, nil)
		first.advance(60_000)
		b := first.kept.bucket()
		is.True(t, b.From >= 60_000, b.From)

		second := startKeeping(t, `"seed": 8, `+pageSettings, &b)
		started := find(simulator.Response{Events: second.events}, func(e simulator.Event) bool { return e.Kind == "process" && e.What == "started" })
		is.True(t, len(started) > 0)
		is.True(t, *started[0].Wall >= b.From, "the wall clock goes on from the history")
		s := second.do("now", "{}").State
		for _, c := range s.Bucket.Commits {
			is.True(t, c.At <= 0, "a commit object from before is from before the start")
			is.Equal(t, "", c.Process)
		}
		seeded := find(simulator.Response{Events: second.events}, func(e simulator.Event) bool { return e.Kind == "store" && e.What == "seeded" })
		is.Equal(t, 1, len(seeded))
		is.Equal(t, s.Bucket.ManifestVersion, seeded[0].Version)
	})

	t.Run("should refuse a bucket that is damaged, and leave the engine as it was", func(t *testing.T) {
		first := startKeeping(t, `"seed": 7, `+pageSettings, nil)
		for range 4 {
			first.advance(15_000)
		}
		good := first.kept.bucket()
		manifest := slices.IndexFunc(good.Objects, func(o simulator.StoredObject) bool { return strings.HasSuffix(o.Key, "/manifest.json") })
		is.True(t, manifest >= 0)
		head := first.do("now", "{}").State.Bucket.Manifest.Head

		tests := []struct {
			name, error string
			damage      func(b *simulator.StoredBucket)
		}{
			{"another version", "bucket.version", func(b *simulator.StoredBucket) { b.Version = "0-0" }},
			{"the head missing", "which the bucket lacks", func(b *simulator.StoredBucket) {
				b.Objects = slices.DeleteFunc(b.Objects, func(o simulator.StoredObject) bool { return strings.HasSuffix(o.Key, head) })
			}},
			{"a commit object in the middle of the log missing", "which the bucket lacks", func(b *simulator.StoredBucket) {
				b.Objects = slices.Delete(b.Objects, 1, 2)
			}},
			{"a manifest that is not one", "the manifest cannot be read", func(b *simulator.StoredBucket) {
				b.Objects[manifest].Content = []byte("{}")
			}},
			{"a commit object with a byte changed", "cannot be read", func(b *simulator.StoredBucket) {
				b.Objects[0].Content[len(b.Objects[0].Content)-1] ^= 1
			}},
			{"a key outside the prefix", "neither the manifest nor a commit object", func(b *simulator.StoredBucket) {
				b.Objects[0].Key = "other/" + b.Objects[0].Key
			}},
			{"a counter behind the log", "which the counter", func(b *simulator.StoredBucket) { b.Values = 1 }},
			{"a key twice", "twice", func(b *simulator.StoredBucket) { b.Objects = append(b.Objects, b.Objects[0]) }},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				var b simulator.StoredBucket
				data, err := json.Marshal(good)
				is.NotError(t, err)
				is.NotError(t, json.Unmarshal(data, &b))
				test.damage(&b)
				data, err = json.Marshal(b)
				is.NotError(t, err)
				a := first.try("start", `{"seed": 8, `+pageSettings+`, "bucket": `+string(data)+`}`)
				is.True(t, strings.Contains(a.Error, test.error), a.Error)
				// The session that runs goes on.
				first.advance(1000)
			})
		}
		// The bucket itself is taken.
		data, err := json.Marshal(good)
		is.NotError(t, err)
		is.Equal(t, "", first.try("start", `{"seed": 8, `+pageSettings+`, "bucket": `+string(data)+`}`).Error)
	})

	t.Run("should refuse a bucket that the store does not take, under faults that make every write fail", func(t *testing.T) {
		first := startKeeping(t, `"seed": 7, `+pageSettings, nil)
		first.advance(30_000)
		data, err := json.Marshal(first.kept.bucket())
		is.NotError(t, err)
		a := first.try("start", `{"seed": 8, `+pageSettings+`, "faults": {"lostRequest": 1}, "bucket": `+string(data)+`}`)
		is.True(t, strings.Contains(a.Error, "the store did not take"), a.Error)
		var res simulator.Response
		is.NotError(t, json.Unmarshal((&simulator.Engine{}).Handle(t, "run", []byte(`{"faults": {"lostRequest": 1}, "bucket": `+string(data)+`}`)), &res))
		is.True(t, strings.Contains(res.Error, "the store did not take"), res.Error)
		is.Equal(t, 0, len(res.Events))
	})

	t.Run("should keep the objects of a database under its prefix", func(t *testing.T) {
		k := startKeeping(t, `"seed": 7, "prefix": "db-x7k2", `+pageSettings, nil)
		k.advance(30_000)
		is.True(t, len(k.kept.objects) > 1)
		for key := range k.kept.objects {
			is.True(t, strings.HasPrefix(key, "db-x7k2/"), key)
		}
		b := k.kept.bucket()
		data, err := json.Marshal(b)
		is.NotError(t, err)
		a := k.try("start", `{"seed": 8, "prefix": "db", `+pageSettings+`, "bucket": `+string(data)+`}`)
		is.True(t, strings.Contains(a.Error, "neither the manifest nor a commit object"), a.Error)
		is.True(t, strings.Contains(k.try("start", `{"prefix": "DB/x"}`).Error, "prefix is"))
	})
}
