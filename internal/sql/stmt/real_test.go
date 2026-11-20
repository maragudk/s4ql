package stmt_test

import (
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// realText at random, the text of a real literal of SQLite without a sign, made to be hard to round:
// many digits, more than the 19 that SQLite reads, digits halfway between two floats and just beside
// that, runs of nines and zeros, and exponents at and beyond the limits of float64.
func realText(r *rand.Rand) string {
	digits := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(byte('0' + r.IntN(10)))
		}
		return b.String()
	}
	switch r.IntN(6) {
	case 0:
		// Digits of any length, with a point anywhere, and maybe an exponent.
		whole, frac := digits(r.IntN(30)), digits(r.IntN(30))
		if whole == "" && frac == "" {
			frac = "5"
		}
		text := whole + "." + frac
		if r.IntN(2) == 0 {
			text += []string{"e", "E", "e+", "E-", "e-"}[r.IntN(5)] + strconv.Itoa(r.IntN(400))
		}
		return text
	case 1:
		// A float's exact value halfway to the next one, cut to between 15 and 40 significant digits, and
		// with one more or one less in its last digit.
		f := math.Float64frombits(r.Uint64() >> 1)
		if math.IsInf(f, 0) || math.IsNaN(f) || f == math.MaxFloat64 {
			f = 1
		}
		a := new(big.Float).SetPrec(2200).SetFloat64(f)
		b := new(big.Float).SetPrec(2200).SetFloat64(math.Nextafter(f, math.Inf(1)))
		mid := new(big.Float).SetPrec(2200).Add(a, b)
		mid.Quo(mid, big.NewFloat(2))
		mantissa, exp, _ := strings.Cut(mid.Text('e', 15+r.IntN(25)), "e")
		if r.IntN(3) == 0 {
			last := mantissa[len(mantissa)-1]
			if last > '0' && last < '9' {
				mantissa = mantissa[:len(mantissa)-1] + string(last+byte(r.IntN(3))-1)
			}
		}
		return mantissa + "e" + exp
	case 2:
		// Runs of nines or zeros.
		run := strings.Repeat([]string{"9", "0"}[r.IntN(2)], 1+r.IntN(30))
		return "1" + run + "." + run + "1e" + strconv.Itoa(r.IntN(700)-350)
	case 3:
		// Around the largest float and the smallest ones.
		return []string{"1.7976931348623157", "1.7976931348623158", "1.7976931348623159", "2.4703282292062327",
			"2.4703282292062328", "4.9406564584124654", "2.2250738585072011", "2.2250738585072014"}[r.IntN(8)] +
			digits(r.IntN(10)) + "e" + []string{"308", "-324", "-308", "-323", "309", "-325"}[r.IntN(6)]
	case 4:
		// Leading zeros, and digits that start beyond the 19th place after them.
		return strings.Repeat("0", r.IntN(30)) + "." + strings.Repeat("0", r.IntN(30)) + digits(1+r.IntN(25)) +
			"e" + strconv.Itoa(r.IntN(100)-50)
	}
	// An exponent far out of range, or with many digits.
	return digits(1+r.IntN(5)) + "." + digits(r.IntN(5)) + "e" + []string{"", "+", "-"}[r.IntN(3)] +
		strings.Repeat("0", r.IntN(10)) + strconv.Itoa(r.IntN([]int{20000, 1e9}[r.IntN(2)]))
}

// realOfLiteral checks the real literal lit, with an optional sign, against SQLite, on the database d:
// the pass gives the value that SQLite gives for `select lit`, bit for bit, or rejects a literal that
// SQLite reads as an infinity, and gives how it answered.
func realOfLiteral(t testing.TB, d *db, lit string) string {
	t.Helper()
	src := "insert into t (v) values (" + lit + ")"
	s, err := stmt.Check(src)
	sqlite := d.value("select " + lit)
	if err != nil {
		f, ok := sqlite.(stmt.Real)
		if !ok || !math.IsInf(float64(f), 0) || !strings.Contains(err.Error(), "infinite real number") {
			t.Fatalf("%q: gives %v, and SQLite %v", lit, err, show(sqlite))
		}
		return "infinite"
	}
	got := s.(*stmt.Insert).Values[0].Value
	if show(got) != show(sqlite) {
		t.Fatalf("%q: gives %v, and SQLite %v", lit, show(got), show(sqlite))
	}
	return "real"
}

// TestCheck_reals checks the value of real literals against SQLite, for texts made to be hard to
// round. The routine of SQLite is not Go's, which rounds every text to the nearest float, and the
// test logs for how many texts they differ.
func TestCheck_reals(t *testing.T) {
	n := 100000
	if slow() {
		n /= 10
	}
	r := rand.New(rand.NewPCG(*seed, 0x5ea1))
	d := open(t)
	answers := map[string]int{}
	differs := 0
	for range n {
		text := realText(r)
		lit := text
		if r.IntN(3) == 0 {
			lit = "-" + text
		}
		answers[realOfLiteral(t, d, lit)]++
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			if v, ok := d.value("select " + text).(stmt.Real); ok && math.Float64bits(float64(v)) != math.Float64bits(f) {
				differs++
			}
		}
	}
	t.Logf("%v texts: %v real, %v infinite; on %v of them SQLite gives another float than Go's strconv.ParseFloat",
		n, answers["real"], answers["infinite"], differs)
	is.True(t, answers["real"] > n/2 && answers["infinite"] > 0)
}

// FuzzLiteral checks the value of any literal that the pass supports as a value of an insert against
// the value that SQLite gives it. A text with commas is several values.
func FuzzLiteral(f *testing.F) {
	for _, c := range checkCases {
		if s, ok := c.want.(*stmt.Insert); ok {
			for _, v := range s.Values {
				if v.Parameter == 0 {
					f.Add(c.src[v.Span.Start:v.Span.End])
				}
			}
		}
	}
	r := rand.New(rand.NewPCG(1, 0x5ea1))
	for range 100 {
		f.Add(realText(r))
		for _, t := range stmttest.Types[1:] {
			text, _ := stmttest.Literal(r, stmttest.ValueOf(r, t))
			f.Add(text)
		}
	}
	nullText, _ := stmttest.Literal(r, stmt.Null{})
	f.Add(nullText)
	f.Fuzz(func(t *testing.T, lit string) {
		src := "insert into t (v) values (" + lit + ")"
		s, err := stmt.Check(src)
		if err != nil {
			return
		}
		insert, ok := s.(*stmt.Insert)
		if !ok {
			return
		}
		d := openUnmanaged(t)
		defer func() { _ = d.c.Close() }()
		for _, v := range insert.Values {
			if v.Parameter > 0 {
				continue
			}
			value := src[v.Span.Start:v.Span.End]
			if got, want := show(v.Value), show(d.value("select "+value)); got != want {
				t.Fatalf("%q: gives %v, and SQLite %v", value, got, want)
			}
		}
	})
}
