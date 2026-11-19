//go:build s4ql_browser

package simulator_test

import (
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/simulator"
)

func TestParseRequest(t *testing.T) {
	t.Run("should give every missing field its default", func(t *testing.T) {
		r, err := simulator.ParseRequest([]byte(`{}`))
		is.NotError(t, err)
		is.Equal(t, uint64(1), r.Seed)
		is.Equal(t, 120_000.0, r.End)
		is.Equal(t, 3, r.Instances)
		is.EqualSlice(t, []float64{30_000}, r.Leases)
		is.Equal(t, 50.0, r.Latency)
		is.True(t, r.Workload.On)
		is.Equal(t, 2, r.Workload.Callers)
		is.Equal(t, 40, r.Workload.Writes)
		is.Equal(t, 0.0, r.Faults.LostRequest)
		is.Equal(t, "", r.Client.Instance)
		is.True(t, !r.Standby, "no standby")
	})

	t.Run("should take a client and a standby", func(t *testing.T) {
		r, err := simulator.ParseRequest([]byte(`{"instances": 2, "client": {"instance": "b"}, "standby": true, "schedule": [{"at": 1, "do": "client", "instance": "a"}]}`))
		is.NotError(t, err)
		is.Equal(t, "b", r.Client.Instance)
		is.True(t, r.Standby, "standby")
	})

	t.Run("should keep the defaults of the fields of an object that is given in part", func(t *testing.T) {
		r, err := simulator.ParseRequest([]byte(`{"workload": {"callers": 1}}`))
		is.NotError(t, err)
		is.Equal(t, 1, r.Workload.Callers)
		is.Equal(t, 40, r.Workload.Writes)
		is.True(t, r.Workload.On)
	})

	t.Run("should replace the default leases with those given", func(t *testing.T) {
		r, err := simulator.ParseRequest([]byte(`{"leases": [20000, 45000]}`))
		is.NotError(t, err)
		is.EqualSlice(t, []float64{20_000, 45_000}, r.Leases)
	})

	tests := []struct {
		name, request, want string
	}{
		{"should refuse text that is not JSON", `{"seed": `, "not JSON of a request"},
		{"should refuse a member that is no field", `{"seeds": 2}`, "not JSON of a request"},
		{"should refuse a field of the wrong type", `{"end": "soon"}`, "not JSON of a request"},
		{"should refuse an end of zero", `{"end": 0}`, "end is 0"},
		{"should refuse an end beyond the limit", `{"end": 99999999}`, "end is 9.9999999e+07"},
		{"should refuse no instances", `{"instances": 0}`, "instances is 0"},
		{"should refuse too many instances", `{"instances": 6}`, "instances is 6"},
		{"should refuse a lease that is too short", `{"leases": [10]}`, "a lease of 10"},
		{"should refuse no leases", `{"leases": []}`, "leases is empty"},
		{"should refuse a probability above one", `{"faults": {"lostRequest": 2}}`, "faults.lostRequest is 2"},
		{"should refuse a negative duration of a fault", `{"faults": {"slowBy": -1}}`, "faults.slowBy is -1"},
		{"should refuse a share of writes above 100", `{"workload": {"writes": 101}}`, "workload.writes is 101"},
		{"should refuse an unknown action", `{"schedule": [{"at": 1, "do": "explode"}]}`, `schedule[0]: no action is called "explode"`},
		{"should refuse an action at the end", `{"end": 1000, "schedule": [{"at": 1000, "do": "calm"}]}`, "schedule[0]: calm at 1000"},
		{"should refuse an action without its instance", `{"schedule": [{"at": 1, "do": "crash"}]}`, `crash needs an instance from a to c, and has ""`},
		{"should refuse an instance that the run does not have", `{"instances": 2, "schedule": [{"at": 1, "do": "crash", "instance": "c"}]}`, `crash needs an instance from a to b, and has "c"`},
		{"should refuse an instance for an action of the store", `{"schedule": [{"at": 1, "do": "calm", "instance": "a"}]}`, `calm is for no instance`},
		{"should refuse a cut-off without its duration", `{"schedule": [{"at": 1, "do": "cutOff", "instance": "a"}]}`, "cutOff needs for"},
		{"should refuse a shift of the wall clock by nothing", `{"schedule": [{"at": 1, "do": "shiftWall", "instance": "a"}]}`, "shiftWall needs by"},
		{"should refuse a client of an instance that the run does not have", `{"instances": 2, "client": {"instance": "c"}}`, `client.instance is "c", and must be empty or an instance from a to b`},
		{"should refuse a switch of the client without its instance", `{"client": {"instance": "a"}, "schedule": [{"at": 1, "do": "client"}]}`, `client needs an instance from a to c, and has ""`},
		{"should refuse a lost answer for an unknown request", `{"schedule": [{"at": 1, "do": "loseAnswer", "request": "all"}]}`, `loseAnswer needs request`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := simulator.ParseRequest([]byte(test.request))
			is.True(t, err != nil, "an error")
			is.True(t, strings.Contains(err.Error(), test.want), err.Error())
		})
	}

	t.Run("should list every problem of a request", func(t *testing.T) {
		_, err := simulator.ParseRequest([]byte(`{"instances": 0, "latency": -1}`))
		is.True(t, err != nil, "an error")
		is.True(t, strings.Contains(err.Error(), "instances is 0") && strings.Contains(err.Error(), "latency is -1"), err.Error())
	})
}
