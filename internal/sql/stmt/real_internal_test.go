package stmt

import (
	"fmt"
	"math"
	"math/big"
	"math/rand/v2"
	"testing"

	"maragu.dev/is"
)

// topBits of the positive number x: its 96 most significant bits, as the 64 of the first and the 32
// after them, rounded to the nearest if round, and cut otherwise.
func topBits(x *big.Rat, round bool) (uint64, uint32) {
	// y = x * 2^k, from 2^95 to 2^96.
	two := big.NewRat(2, 1)
	low, high := new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 95)), new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))
	y := new(big.Rat).Set(x)
	for y.Cmp(low) < 0 {
		y.Mul(y, two)
	}
	for y.Cmp(high) >= 0 {
		y.Quo(y, two)
	}
	m, rest := new(big.Int).QuoRem(y.Num(), y.Denom(), new(big.Int))
	if round && new(big.Int).Lsh(rest, 1).Cmp(y.Denom()) >= 0 {
		m.Add(m, big.NewInt(1))
	}
	return new(big.Int).Rsh(m, 32).Uint64(), uint32(new(big.Int).And(m, big.NewInt(math.MaxUint32)).Uint64())
}

// pow10 is 10^p, exactly.
func pow10(p int) *big.Rat {
	ten := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(max(p, -p))), nil)
	if p < 0 {
		return new(big.Rat).SetFrac(big.NewInt(1), ten)
	}
	return new(big.Rat).SetInt(ten)
}

func TestPowerOfTen(t *testing.T) {
	t.Run("should hold 10^n for n from 0 to 26 in the 64 bits of its first table, exactly", func(t *testing.T) {
		for n, hi := range powerOfTenBase {
			want, lo := topBits(pow10(n), false)
			is.Equal(t, want, hi, fmt.Sprint("1e", n))
			is.Equal(t, uint32(0), lo, fmt.Sprint("1e", n, " is exact in 64 bits"))
		}
	})

	t.Run("should hold the 96 most significant bits of 10^(27g) in its other two tables, rounded to the nearest, and those of 10^-1 cut", func(t *testing.T) {
		is.Equal(t, len(powerOfTenScale), len(powerOfTenScaleLo))
		for g := range powerOfTenScale {
			p, round := 27*(g-13), true
			if g == 13 {
				// SQLite's special case, which is cut where rounding would give 0xcccccccd.
				p, round = -1, false
			}
			hi, lo := topBits(pow10(p), round)
			is.Equal(t, hi, powerOfTenScale[g], fmt.Sprint("1e", p))
			is.Equal(t, lo, powerOfTenScaleLo[g], fmt.Sprint("1e", p))
		}
	})
}

func TestMultiply160(t *testing.T) {
	t.Run("should give the 96 bits of the product of a 96-bit number and a 64-bit one that follow its lowest 64", func(t *testing.T) {
		exact := func(a uint64, aLo uint32, b uint64) (uint64, uint32) {
			x := new(big.Int).Or(new(big.Int).Lsh(new(big.Int).SetUint64(a), 32), big.NewInt(int64(aLo)))
			x.Mul(x, new(big.Int).SetUint64(b))
			x.Rsh(x, 64)
			return new(big.Int).Rsh(x, 32).Uint64(), uint32(new(big.Int).And(x, big.NewInt(math.MaxUint32)).Uint64())
		}
		cases := [][3]uint64{
			{0, 0, 0}, {math.MaxUint64, math.MaxUint32, math.MaxUint64}, {1, 0, 1 << 63}, {1 << 63, 1 << 31, 3},
			// The low 32 bits of the product shifted by 32 are all ones, so that a carry into them shows.
			{0, 1, math.MaxUint64},
		}
		r := rand.New(rand.NewPCG(1, 160))
		for range 10000 {
			cases = append(cases, [3]uint64{r.Uint64(), uint64(r.Uint32()), r.Uint64()})
		}
		for _, c := range cases {
			hi, lo := multiply160(c[0], uint32(c[1]), c[2])
			wantHi, wantLo := exact(c[0], uint32(c[1]), c[2])
			is.Equal(t, wantHi, hi, fmt.Sprint(c))
			is.Equal(t, wantLo, lo, fmt.Sprint(c))
		}
	})
}
