package skips

import (
	"os"
	"testing"
)

func TestBare(t *testing.T) {
	t.Skip("not now") // want `Skip skips the test other than in if testing\.Short\(\) \{ \.\.\. \}`
}

func TestVariable(t *testing.T) {
	if os.Getenv("S4QL_SOMETHING") == "" {
		t.Skipf("%v is not set", "S4QL_SOMETHING") // want `Skipf skips the test`
	}
}

func TestSkipNow(t *testing.T) {
	t.SkipNow() // want `SkipNow skips the test`
}

func BenchmarkSkip(b *testing.B) {
	b.Skip() // want `Skip skips the test`
}

func FuzzSkip(f *testing.F) {
	f.Skip() // want `Skip skips the test`
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 10 {
			t.Skip() // want `Skip skips the test`
		}
	})
}

func TestNotShort(t *testing.T) {
	if !testing.Short() {
		t.Skip() // want `Skip skips the test`
	}
}

func TestShortAndMore(t *testing.T) {
	if testing.Short() && os.Getenv("CI") != "" {
		t.Skip() // want `Skip skips the test`
	}
}

func TestElse(t *testing.T) {
	if testing.Short() {
		t.Log("short")
	} else {
		t.Skip() // want `Skip skips the test`
	}
}

func TestInit(t *testing.T) {
	if short := testing.Short(); short {
		t.Skip() // want `Skip skips the test`
	}
}

func TestNested(t *testing.T) {
	if testing.Short() {
		for range 1 {
			t.Skip() // want `Skip skips the test`
		}
	}
}

func TestDeferred(t *testing.T) {
	if testing.Short() {
		defer t.Skip() // want `Skip skips the test`
	}
}

func TestMethodValue(t *testing.T) {
	skip := t.Skip // want `Skip skips the test`
	skip()
}

func TestMethodExpression(t *testing.T) {
	(*testing.T).SkipNow(t) // want `SkipNow skips the test`
}

func TestInterface(t *testing.T) {
	var tb testing.TB = t
	tb.Skip() // want `Skip skips the test`
}

// wrapped embeds a *testing.T, so its Skip is that of testing.
type wrapped struct {
	*testing.T
}

func TestWrapped(t *testing.T) {
	w := wrapped{t}
	w.Skip() // want `Skip skips the test`
}

// Short is not testing.Short.
func Short() bool {
	return true
}

func TestOtherShort(t *testing.T) {
	if Short() {
		t.Skip() // want `Skip skips the test`
	}
}

// own has a method Skip of its own, which is not of the package testing.
type own struct{}

func (own) Skip() {}

func TestOwnSkip(t *testing.T) {
	if os.Getenv("S4QL_SOMETHING") == "" {
		own{}.Skip()
	}
}

func TestShort(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a bucket")
	}
}

func TestShortInParentheses(t *testing.T) {
	if (testing.Short()) {
		t.Log("short")
		t.Skipf("needs %v", "a bucket")
	}
}

func TestShortInSubtest(t *testing.T) {
	t.Run("a subtest", func(t *testing.T) {
		if testing.Short() {
			t.SkipNow()
		}
	})
}

func BenchmarkShort(b *testing.B) {
	if testing.Short() {
		b.Skip()
	}
}

func TestShortOnInterface(t *testing.T) {
	var tb testing.TB = t
	if testing.Short() {
		tb.Skip()
	}
}

func TestHelper(t *testing.T) {
	needsBucket(t)
}
