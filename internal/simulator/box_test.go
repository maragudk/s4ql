//go:build s4ql_browser

package simulator_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

// sql sends the statement of the box in the session k, advances until it has ended, and returns the
// event that ends its call, and the answer of the advance.
func (k *keeping) sql(text string) (simulator.Event, simulator.Live) {
	k.t.Helper()
	body, err := json.Marshal(simulator.Action{Do: "sql", SQL: text})
	is.NotError(k.t, err)
	k.act(string(body))
	from := len(k.events)
	a := k.do("advance", `{"by": 120000, "until": "sql"}`)
	k.kept.keep(a)
	for _, e := range k.events[from:] {
		if e.Kind == "call" && e.Caller == "box" && e.What != "started" {
			return e, a
		}
	}
	k.t.Fatalf("the statement %q has not ended after the advance", text)
	return simulator.Event{}, a
}

// served starts a session with the page's settings on the bucket b, or none, and advances it until a
// serves.
func served(t *testing.T, seed int, b *simulator.StoredBucket) *keeping {
	t.Helper()
	k := startKeeping(t, fmt.Sprintf(`"seed": %v, `+pageSettings, seed), b)
	for range 60 {
		k.advance(5_000)
		for _, e := range k.events {
			if e.Kind == "call" && e.Caller == "client" && e.What == "committed" {
				return k
			}
		}
	}
	t.Fatal("a commits no request in five minutes")
	return k
}

// values of the cells of the rows, as text, with null for NULL.
func values(rows [][]*simulator.Cell) []string {
	var out []string
	for _, row := range rows {
		var cells []string
		for _, c := range row {
			if c == nil {
				cells = append(cells, "null")
				continue
			}
			cells = append(cells, c.Type+":"+c.Value)
		}
		out = append(out, strings.Join(cells, " "))
	}
	return out
}

func TestEngine_box(t *testing.T) {
	t.Run("should run the visitor's statements on the instance that the requests go to, each write as a commit object of its own", func(t *testing.T) {
		k := served(t, 3, nil)

		e, _ := k.sql("create table t (k integer primary key, v text, r real, b blob) strict, without rowid")
		is.Equal(t, "committed", e.What, e.Error)
		is.Equal(t, "a", e.Instance)
		is.Equal(t, "sql", e.Op)
		is.True(t, e.Commit != "", "the create has a commit object")
		is.True(t, e.Affected == nil, "a create counts no rows")

		for i, v := range []string{"'one', 1.5, x'00ff'", "null, 2.0, null", "'three', null, x''"} {
			e, _ = k.sql(fmt.Sprintf("insert into t values (%v, %v)", i+1, v))
			is.Equal(t, "committed", e.What, e.Error)
			is.True(t, e.Affected != nil && *e.Affected == 1, "an insert of one row")
		}

		e, _ = k.sql("select k, v, r, b from t order by k desc limit 2")
		is.Equal(t, "served", e.What, e.Error)
		is.EqualSlice(t, []string{"k", "v", "r", "b"}, e.Columns)
		is.EqualSlice(t, []string{"integer:3 text:three null blob:", "integer:2 null real:2.0 null"}, values(e.Rows))
		is.Equal(t, 2, e.Count)
		is.True(t, e.Position != "", "a select has its position")

		e, _ = k.sql("update t set v = 'many' where k > 1")
		is.Equal(t, "committed", e.What, e.Error)
		is.True(t, e.Affected != nil && *e.Affected == 2, "an update of two rows")

		e, _ = k.sql("delete from t where k = 9")
		is.Equal(t, "committed", e.What, e.Error)
		is.True(t, e.Affected != nil && *e.Affected == 0, "a delete of no row")
		is.Equal(t, "", e.Commit, "a write that changes nothing has no commit object")
		e, _ = k.sql("create table if not exists t (k integer primary key) strict, without rowid")
		is.Equal(t, "committed", e.What, e.Error)
		is.Equal(t, "", e.Commit, "a statement that does nothing has no commit object")

		e, _ = k.sql("select * from barcodes")
		is.Equal(t, "served", e.What, e.Error)
		is.EqualSlice(t, []string{"code"}, e.Columns)
		is.True(t, e.Count > 0 && len(e.Rows) == min(e.Count, 50), fmt.Sprint(len(e.Rows), " of ", e.Count))

		// The engine's errors, in its own words.
		e, _ = k.sql("insert into t values (1, 'again', null, null)")
		is.Equal(t, "abandoned", e.What)
		is.Equal(t, "UNIQUE constraint failed: t.k", e.Error)
		e, _ = k.sql("select missing from t")
		is.Equal(t, "failed", e.What)
		is.Equal(t, "no such column: missing", e.Error)
		e, _ = k.sql("selec 1")
		is.Equal(t, "failed", e.What)
		is.Equal(t, `near "selec": syntax error`, e.Error)

		// The commit objects of the box are in the log, each with its statement.
		state := k.do("now", "{}").State
		statements := map[string]bool{}
		for _, c := range state.Bucket.Commits {
			if c.InLog {
				for _, s := range c.Statements {
					statements[s.SQL] = true
				}
			}
		}
		is.True(t, statements["update t set v = 'many' where k > 1"], "the update is in the log")
		is.True(t, statements["insert into t values (2, null, 2.0, null)"], "an insert is in the log")

		last := k.end()
		is.Equal(t, 0, len(violations(append(k.events, last.Events...))))
		is.True(t, last.Summary.Calls["sql committed"] >= 6, fmt.Sprint(last.Summary.Calls))
	})

	t.Run("should refuse every statement but a select that names the table of the app, before it reaches the engine", func(t *testing.T) {
		k := served(t, 4, nil)
		for _, text := range []string{
			"insert into barcodes (code) values (1)", "delete from barcodes", "update BarCodes set code = 1",
			`drop table "barcodes"`, "create table if not exists barcodes (k integer primary key) strict, without rowid",
			"create index i on barcodes (code)", "create index barcodes on t (k)", "drop index barcodes",
		} {
			body, err := json.Marshal(simulator.Action{Do: "sql", SQL: text})
			is.NotError(t, err)
			a := k.act(string(body))
			is.Equal(t, 1, len(a.Events), text)
			is.Equal(t, "ignored", a.Events[0].What, text)
			is.Equal(t, "barcodes is the app's table, so the box can only select from it", a.Events[0].Text)
			is.Equal(t, text, a.Events[0].SQL)
		}
		a := k.advance(10_000)
		for _, e := range a.Events {
			is.True(t, e.Caller != "box", "no call of the box")
		}
		e, _ := k.sql("select code from barcodes order by code desc limit 5")
		is.Equal(t, "served", e.What, e.Error)
		is.True(t, len(e.Rows) > 0 && len(e.Rows) <= 5, fmt.Sprint(len(e.Rows), " rows"))
	})

	t.Run("should take one statement at a time, and stop an advance until it ends once it has", func(t *testing.T) {
		k := served(t, 5, nil)
		// Without a statement on its way, the advance moves nothing.
		before := k.last.T
		a := k.do("advance", `{"by": 60000, "until": "sql"}`)
		is.Equal(t, before, a.T)

		a = k.act(`{"do": "sql", "sql": "create table u (k integer primary key) strict, without rowid"}`)
		is.Equal(t, "done", a.Events[0].What)
		is.Equal(t, "a", a.Events[0].Instance)
		a = k.act(`{"do": "sql", "sql": "select * from u"}`)
		is.Equal(t, "ignored", a.Events[0].What)
		is.Equal(t, "a statement of the box is on its way", a.Events[0].Text)

		from := len(k.events)
		a = k.do("advance", `{"by": 60000, "until": "sql"}`)
		var end simulator.Event
		for _, e := range k.events[from:] {
			if e.Caller == "box" && e.What != "started" {
				end = e
			}
		}
		is.Equal(t, "committed", end.What, end.Error)
		is.True(t, a.T >= end.T && a.T < end.T+1000, fmt.Sprint("the advance stops at ", a.T, " just after the end at ", end.T))
		is.True(t, a.T < before+60_000, "far before the end of the advance")

		// A switch of the requests right after the action leaves the statement on the instance that
		// its event names.
		a = k.act(`{"do": "sql", "sql": "select * from u"}`)
		is.Equal(t, "a", a.Events[0].Instance)
		k.act(`{"do": "client", "instance": "b"}`)
		from = len(k.events)
		k.do("advance", `{"by": 60000, "until": "sql"}`)
		for _, e := range k.events[from:] {
			if e.Caller == "box" && e.What != "started" {
				end = e
			}
		}
		is.Equal(t, "a", end.Instance)
		is.Equal(t, "served", end.What, end.Error)

		// An empty statement gets the engine's own error.
		k.act(`{"do": "client", "instance": "a"}`)
		e, _ := k.sql("  ")
		is.Equal(t, "failed", e.What)
		is.Equal(t, "s4ql: the text holds no statement", e.Error)
	})

	t.Run("should keep the outcomes of a write apart under the faults of the instance", func(t *testing.T) {
		k := served(t, 6, nil)
		e, _ := k.sql("create table t (k integer primary key) strict, without rowid")
		is.Equal(t, "committed", e.What, e.Error)

		// While the connection to S3 hangs, the write waits, and is not committed at its timeout.
		k.act(`{"do": "hangNetwork", "instance": "a", "on": true}`)
		k.act(`{"do": "sql", "sql": "insert into t values (1)"}`)
		a := k.advance(10_000)
		for _, e := range a.Events {
			is.True(t, e.Caller != "box" || e.What == "started", "the write waits for S3")
		}
		e, _ = k.sql("select 1")
		is.Equal(t, "not_committed", e.What, e.Error)
		k.act(`{"do": "hangNetwork", "instance": "a"}`)

		// A crashed instance does not take the statement.
		k.act(`{"do": "crash", "instance": "a"}`)
		e, _ = k.sql("insert into t values (2)")
		is.Equal(t, "down", e.What)
		k.act(`{"do": "restart", "instance": "a"}`)

		// The other instance refuses it while a holds the lease.
		k.act(`{"do": "client", "instance": "b"}`)
		e, _ = k.sql("insert into t values (3)")
		is.Equal(t, "refused", e.What, e.Error)
		e, _ = k.sql("select * from t")
		is.Equal(t, "refused", e.What, e.Error)

		last := k.end()
		is.Equal(t, 0, len(violations(append(k.events, last.Events...))))
	})

	t.Run("should keep the visitor's tables in the bucket across a reload", func(t *testing.T) {
		first := served(t, 7, nil)
		for _, text := range []string{"create table mine (k integer primary key, v text) strict, without rowid", "insert into mine values (1, 'kept')", "create index mine_v on mine (v)"} {
			e, _ := first.sql(text)
			is.Equal(t, "committed", e.What, e.Error)
		}
		first.advance(10_000)
		b := first.kept.bucket()

		second := startKeeping(t, `"seed": 9, `+pageSettings, &b)
		var e simulator.Event
		for range 20 {
			if e, _ = second.sql("select v from mine where v = 'kept'"); e.What == "served" {
				break
			}
			second.advance(10_000)
		}
		is.Equal(t, "served", e.What, e.Error)
		is.EqualSlice(t, []string{"text:kept"}, values(e.Rows))
		last := second.end()
		is.Equal(t, 0, len(violations(append(second.events, last.Events...))))
	})

	t.Run("should give the same events and hash in a session as a batch run with the same statements", func(t *testing.T) {
		schedule := `[{"at": 50000, "do": "sql", "sql": "create table t (k integer primary key) strict, without rowid"},
			{"at": 52000, "do": "sql", "sql": "insert into t values (1)"},
			{"at": 54000, "do": "hangNetwork", "instance": "a", "on": true}, {"at": 54100, "do": "sql", "sql": "insert into t values (2)"},
			{"at": 70000, "do": "hangNetwork", "instance": "a"}, {"at": 90000, "do": "sql", "sql": "select * from t"},
			{"at": 92000, "do": "sql", "sql": "insert into barcodes values (1)"}, {"at": 92000, "do": "sql", "sql": "drop table t"}]`
		settings := `"seed": 2, "instances": 2, ` + pageSettings
		res := run(t, &simulator.Engine{}, `{`+settings+`, "end": 120000, "schedule": `+schedule+`}`)
		l, _ := replay(t, `{`+settings+`}`, res.Request.Schedule, 120_000)
		is.Equal(t, res.Hash, l.last.Hash)
		is.EqualSlice(t, eventLines(t, res.Events), eventLines(t, append(l.events, l.last.Events...)))
		is.True(t, res.Summary.Calls["sql committed"] >= 2, fmt.Sprint(res.Summary.Calls))
		is.Equal(t, 0, res.Summary.Violations)
	})

	t.Run("should find no violation that is not excused with statements of the box under every fault", func(t *testing.T) {
		for _, seed := range []int{11, 12, 13} {
			k := startKeeping(t, fmt.Sprintf(`"seed": %v, "faults": %v, `+pageSettings, seed, allFaults), nil)
			k.advance(40_000)
			k.sql("create table t (k integer primary key, v integer) strict, without rowid")
			for i := range 12 {
				k.sql(fmt.Sprintf("insert into t values (%v, %v)", i, i*i))
				k.sql("select * from t where k > 3 order by v desc limit 3")
				if i == 6 {
					k.act(`{"do": "crash", "instance": "a"}`)
					k.advance(2_000)
					k.act(`{"do": "restart", "instance": "a"}`)
				}
				k.sql("update t set v = 0 where k < 2")
				k.advance(3_000)
			}
			last := k.end()
			is.Equal(t, 0, len(violations(append(k.events, last.Events...))), fmt.Sprint("seed ", seed))
		}
	})

	t.Run("should refuse a statement longer than its limit", func(t *testing.T) {
		k := startLive(t, `{`+pageSettings+`}`)
		body, err := json.Marshal(simulator.Action{Do: "sql", SQL: "select '" + strings.Repeat("é", 1000) + "'"})
		is.NotError(t, err)
		a := k.try("act", string(body))
		is.Equal(t, "sql has 1009 characters, and may have at most 1000", a.Error)
		a = k.try("act", `{"do": "write", "instance": "a", "sql": "select 1"}`)
		is.Equal(t, "write takes no sql", a.Error)
		a = k.try("act", `{"do": "sql", "instance": "a", "sql": "select 1"}`)
		is.True(t, a.Error != "", "a statement goes where the requests go")
		a = k.try("advance", `{"by": 1, "until": "later"}`)
		is.Equal(t, `until is "later", and must be sql or missing`, a.Error)
	})
}
