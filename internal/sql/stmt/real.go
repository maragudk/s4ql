package stmt

import (
	"math"
	"math/bits"
)

// This file is a port of how SQLite 3.54.1 reads the text of a real literal into a 64-bit float:
// sqlite3AtoF, sqlite3Fp10Convert2, powerOfTen, sqlite3Multiply128, and sqlite3Multiply160 of
// internal/sqlite/c/sqlite3.c, line for line, with the same integer arithmetic. SQLite's routine is
// not Go's strconv.ParseFloat: it keeps only about the first 19 significant digits of a literal, so
// for a longer one it can round to another float than the nearest. A port of the same integer
// arithmetic gives the same bits for every input, which the tests check against SQLite. A new version
// of SQLite that changes these routines needs this file changed with it.

// atof of the text of a real literal of SQLite's tokenizer, as sqlite3AtoF reads it: digits with a
// fraction, an exponent, or both, such as 1.5, .5, 1., or 1e3, without a sign. The result can be
// infinite, as for 1e999, and is never NaN.
func atof(z string) float64 {
	// at gives the byte at i, and 0 past the end, as the zero byte that ends a string in C.
	at := func(i int) byte {
		if i < len(z) {
			return z[i]
		}
		return 0
	}
	isDigit := func(c byte) bool { return '0' <= c && c <= '9' }
	const largest = math.MaxUint64
	var (
		i int
		s uint64 // the mantissa
		d int    // the value is s * 10^d
	)
	// The sign and the spaces that sqlite3AtoF reads never start a token of a real literal, so they
	// are left out. So is what it gives for a text that is not a number, which no such token is.
	if isDigit(at(i)) {
		s = uint64(at(i) - '0')
		i++
		for isDigit(at(i)) {
			s = s*10 + uint64(at(i)-'0')
			i++
			if s >= (largest-9)/10 {
				for isDigit(at(i)) {
					i++
					d++
				}
				break
			}
		}
	}
	if at(i) == '.' {
		i++
		for isDigit(at(i)) {
			if s < (largest-9)/10 {
				s = s*10 + uint64(at(i)-'0')
				d--
			}
			i++
		}
	}
	if c := at(i); c == 'e' || c == 'E' {
		i++
		esign := 1
		switch at(i) {
		case '-':
			esign = -1
			i++
		case '+':
			i++
		}
		if isDigit(at(i)) {
			exp := int(at(i) - '0')
			i++
			for isDigit(at(i)) {
				if exp < 10000 {
					exp = exp*10 + int(at(i)-'0')
				} else {
					exp = 10000
				}
				i++
			}
			d += esign * exp
		}
	}
	if s == 0 {
		return 0
	}
	return fp10Convert2(s, d)
}

// The range of the powers of 10 of [powerOfTen].
const (
	powersOf10First = -348
	powersOf10Last  = 347
)

// u64Bit gives a uint64 with the bit n set.
func u64Bit(n int) uint64 { return 1 << n }

// multiply128 of a and b: the high 64 bits of the product, and the low 64 bits.
func multiply128(a, b uint64) (hi, lo uint64) {
	return bits.Mul64(a, b)
}

// multiply160 of the 96-bit number (a<<32)+aLo and b: the upper 64 bits of the 160-bit product, and
// the 32 bits after them. The lower 64 bits of the product are dropped.
func multiply160(a uint64, aLo uint32, b uint64) (uint64, uint32) {
	// (a<<32 + aLo) * b >> 32 = a*b + (aLo*b)>>32, exactly, as 128 bits.
	hi, lo := bits.Mul64(a, b)
	h2, l2 := bits.Mul64(uint64(aLo), b)
	add := l2>>32 | h2<<32
	lo, carry := bits.Add64(lo, add, 0)
	hi += carry
	// The 128 bits hi:lo are the product shifted right by 32 bits.
	return hi, uint32(lo >> 32)
}

var powerOfTenBase = [...]uint64{
	0x8000000000000000, // 0: 1.0e+0 << 63
	0xa000000000000000, // 1: 1.0e+1 << 60
	0xc800000000000000, // 2: 1.0e+2 << 57
	0xfa00000000000000, // 3: 1.0e+3 << 54
	0x9c40000000000000, // 4: 1.0e+4 << 50
	0xc350000000000000, // 5: 1.0e+5 << 47
	0xf424000000000000, // 6: 1.0e+6 << 44
	0x9896800000000000, // 7: 1.0e+7 << 40
	0xbebc200000000000, // 8: 1.0e+8 << 37
	0xee6b280000000000, // 9: 1.0e+9 << 34
	0x9502f90000000000, // 10: 1.0e+10 << 30
	0xba43b74000000000, // 11: 1.0e+11 << 27
	0xe8d4a51000000000, // 12: 1.0e+12 << 24
	0x9184e72a00000000, // 13: 1.0e+13 << 20
	0xb5e620f480000000, // 14: 1.0e+14 << 17
	0xe35fa931a0000000, // 15: 1.0e+15 << 14
	0x8e1bc9bf04000000, // 16: 1.0e+16 << 10
	0xb1a2bc2ec5000000, // 17: 1.0e+17 << 7
	0xde0b6b3a76400000, // 18: 1.0e+18 << 4
	0x8ac7230489e80000, // 19: 1.0e+19 >> 0
	0xad78ebc5ac620000, // 20: 1.0e+20 >> 3
	0xd8d726b7177a8000, // 21: 1.0e+21 >> 6
	0x878678326eac9000, // 22: 1.0e+22 >> 10
	0xa968163f0a57b400, // 23: 1.0e+23 >> 13
	0xd3c21bcecceda100, // 24: 1.0e+24 >> 16
	0x84595161401484a0, // 25: 1.0e+25 >> 20
	0xa56fa5b99019a5c8, // 26: 1.0e+26 >> 23
}

var powerOfTenScale = [...]uint64{
	0x8049a4ac0c5811ae, // 0: 1.0e-351 << 1229
	0xcf42894a5dce35ea, // 1: 1.0e-324 << 1140
	0xa76c582338ed2621, // 2: 1.0e-297 << 1050
	0x873e4f75e2224e68, // 3: 1.0e-270 << 960
	0xda7f5bf590966848, // 4: 1.0e-243 << 871
	0xb080392cc4349dec, // 5: 1.0e-216 << 781
	0x8e938662882af53e, // 6: 1.0e-189 << 691
	0xe65829b3046b0afa, // 7: 1.0e-162 << 602
	0xba121a4650e4ddeb, // 8: 1.0e-135 << 512
	0x964e858c91ba2655, // 9: 1.0e-108 << 422
	0xf2d56790ab41c2a2, // 10: 1.0e-81 << 333
	0xc428d05aa4751e4c, // 11: 1.0e-54 << 243
	0x9e74d1b791e07e48, // 12: 1.0e-27 << 153
	0xcccccccccccccccc, // 13: 1.0e-1 << 67 (special case)
	0xcecb8f27f4200f3a, // 14: 1.0e+27 >> 26
	0xa70c3c40a64e6c51, // 15: 1.0e+54 >> 116
	0x86f0ac99b4e8dafd, // 16: 1.0e+81 >> 206
	0xda01ee641a708de9, // 17: 1.0e+108 >> 295
	0xb01ae745b101e9e4, // 18: 1.0e+135 >> 385
	0x8e41ade9fbebc27d, // 19: 1.0e+162 >> 475
	0xe5d3ef282a242e81, // 20: 1.0e+189 >> 564
	0xb9a74a0637ce2ee1, // 21: 1.0e+216 >> 654
	0x95f83d0a1fb69cd9, // 22: 1.0e+243 >> 744
	0xf24a01a73cf2dccf, // 23: 1.0e+270 >> 833
	0xc3b8358109e84f07, // 24: 1.0e+297 >> 923
	0x9e19db92b4e31ba9, // 25: 1.0e+324 >> 1013
}

var powerOfTenScaleLo = [...]uint32{
	0x205b896d, // 0: 1.0e-351 << 1229
	0x52064cad, // 1: 1.0e-324 << 1140
	0xaf2af2b8, // 2: 1.0e-297 << 1050
	0x5a7744a7, // 3: 1.0e-270 << 960
	0xaf39a475, // 4: 1.0e-243 << 871
	0xbd8d794e, // 5: 1.0e-216 << 781
	0x547eb47b, // 6: 1.0e-189 << 691
	0x0cb4a5a3, // 7: 1.0e-162 << 602
	0x92f34d62, // 8: 1.0e-135 << 512
	0x3a6a07f9, // 9: 1.0e-108 << 422
	0xfae27299, // 10: 1.0e-81 << 333
	0xaa97e14c, // 11: 1.0e-54 << 243
	0x775ea265, // 12: 1.0e-27 << 153
	0xcccccccc, // 13: 1.0e-1 << 67 (special case)
	0x00000000, // 14: 1.0e+27 >> 26
	0x999090b6, // 15: 1.0e+54 >> 116
	0x69a028bb, // 16: 1.0e+81 >> 206
	0xe80e6f48, // 17: 1.0e+108 >> 295
	0x5ec05dd0, // 18: 1.0e+135 >> 385
	0x14588f14, // 19: 1.0e+162 >> 475
	0x8f1668c9, // 20: 1.0e+189 >> 564
	0x6d953e2c, // 21: 1.0e+216 >> 654
	0x4abdaf10, // 22: 1.0e+243 >> 744
	0xbc633b39, // 23: 1.0e+270 >> 833
	0x0a862f81, // 24: 1.0e+297 >> 923
	0x6c07a2c2, // 25: 1.0e+324 >> 1013
}

// powerOfTen p, for p from [powersOf10First] to [powersOf10Last]: the most significant 64 bits of
// 10^p, and the 32 bits after them.
func powerOfTen(p int) (uint64, uint32) {
	var g, n int
	switch {
	case p < 0:
		if p == -1 {
			return powerOfTenScale[13], powerOfTenScaleLo[13]
		}
		// Go divides and takes the remainder towards zero, as C does.
		g = p / 27
		n = p % 27
		if n != 0 {
			g--
			n += 27
		}
	case p < 27:
		return powerOfTenBase[p], 0
	default:
		g = p / 27
		n = p % 27
	}
	s := powerOfTenScale[g+13]
	if n == 0 {
		return s, powerOfTenScaleLo[g+13]
	}
	x, lo := multiply160(s, powerOfTenScaleLo[g+13], powerOfTenBase[n])
	if u64Bit(63)&x == 0 {
		x = x<<1 | uint64(lo>>31)&1
		lo = lo<<1 | 1
	}
	return x, lo
}

// pwr10to2 of p is floor(log2(10^p)), as SQLite approximates it. The shift to the right rounds a
// negative number down, as in C on the platforms that SQLite runs on.
func pwr10to2(p int) int { return (p * 108853) >> 15 }

// fp10Convert2 of d and p: the float64 that SQLite gives for d * 10^p.
func fp10Convert2(d uint64, p int) float64 {
	if p < powersOf10First {
		return 0
	}
	if p > powersOf10Last {
		return math.Inf(1)
	}
	b := 64 - bits.LeadingZeros64(d)
	lp := pwr10to2(p)
	e := 53 - b - lp
	if e > 1074 {
		if e >= 1130 {
			return 0
		}
		e = 1074
	}
	s := -(e - (64 - b) + lp + 3)
	pwr10h, pwr10l := powerOfTen(p)
	if pwr10l != 0 {
		pwr10h++
		pwr10l = ^pwr10l
	}
	x := d << (64 - b)
	hi, lo := multiply128(x, pwr10h)
	mid1 := uint32(lo >> 32)
	sticky := uint64(1)
	if hi&(u64Bit(s)-1) == 0 {
		h2, _ := multiply128(x, uint64(pwr10l)<<32)
		mid2 := uint32(h2 >> 32)
		if mid1-mid2 > 1 {
			sticky = 1
		} else {
			sticky = 0
		}
		if mid1 < mid2 {
			hi--
		}
	}
	u := hi>>s | sticky
	if u >= u64Bit(55)-2 {
		u = u>>1 | u&1
		e--
	}
	m := (u + 1 + ((u >> 2) & 1)) >> 2
	if e <= -972 {
		return math.Inf(1)
	}
	if m&u64Bit(52) != 0 {
		m = m&^u64Bit(52) | uint64(1075-e)<<52
	}
	return math.Float64frombits(m)
}
