// Package token splits SQL text into tokens exactly as SQLite's tokenizer does.
//
// The promise: for any text, also text that is not UTF-8 or that has NUL bytes in it, [Next] gives
// the same kind and length as sqlite3GetToken of the vendored SQLite, built with the compile options
// of the binding in internal/sqlite, and [Tokens] gives the tokens that sqlite3GetToken gives when it
// is called over the text one token after the other. The tests check this against SQLite's own
// sqlite3GetToken, token by token.
//
// The code follows sqlite3GetToken in tokenize.c (lines 187252-187574 of the vendored sqlite3.c)
// case by case, with SQLite's character classes and its table of keywords, so that the two can be
// read side by side. SQLite reads a NUL-terminated string, and this package reads a byte past the
// end of the text as NUL, so a NUL byte in the text ends it, as it does for SQLite's parser.
//
// The package applies none of s4ql's restrictions: a blob, a bracketed name, or a variable is
// found as SQLite finds it, so that a later layer can reject it where SQLite would have read it.
// A token is either legal or [Illegal]; the package has no error of its own.
//
// What SQLite's parser driver sqlite3RunParser (tokenize.c, sqlite3.c 187579-187742) does on top of
// the tokenizer is left to the parser:
//
//   - It skips [Space] tokens (187649-187652) and [Comment] tokens (187675-187681). Comments are
//     skipped only while SQLITE_DBCONFIG_ENABLE_COMMENTS is on, which is the default; with it off,
//     a comment is an unrecognized token.
//   - It reports an [Illegal] token as `unrecognized token: "X"`, with the token's text, and stops
//     (187682-187688).
//   - It passes a [QNumber] on to the parser, whose action for `term ::= QNUMBER` calls
//     sqlite3DequoteNumber (sqlite3.c 37174), which checks the digit separators and reports a
//     misplaced one as an unrecognized token, without an offset.
//   - It decides from the tokens around them whether [Window], [Over], and [Filter] are keywords or
//     names: analyzeWindowKeyword, analyzeOverKeyword, and analyzeFilterKeyword (187225-187245),
//     called at 187665-187674. They look past spaces and comments, and use the parser's fallback
//     table sqlite3ParserFallback.
//   - At the end of the input, where the next token is [Illegal] with length 0, it feeds the parser
//     a [Semi] of length 0 unless the last token was one, and then the end (187653-187663). A
//     syntax error on that empty token is reported as `incomplete input`.
//   - It stops with SQLITE_TOOBIG when the tokens read add up to more than SQLITE_LIMIT_SQL_LENGTH
//     (187626-187631), and with SQLITE_INTERRUPT on an interrupt (187644-187648).
//
// Also left out: the value of a token. Unquoting strings and names (sqlite3Dequote, sqlite3.c
// 37140), converting numbers and their range (for example "hex literal too big"), reading a blob's
// bytes, the number of a variable, and whether a keyword is read as a name (the parser's %fallback).
// The text of every token is kept, so that a later layer can do these from it.
package token

// Token in SQL text: its kind and where its bytes are. Its text is src[Offset:Offset+Len].
type Token struct {
	Kind   Kind
	Offset int
	Len    int
}

// Text of the token in src, the SQL text it was read from.
func (t Token) Text(src string) string {
	return src[t.Offset : t.Offset+t.Len]
}

// Tokens of src, in order, as [Next] gives them from the start of src, one after the other, until the
// end of src or its first NUL byte. The tokens cover the text before that point without a gap, so if
// the last one does not end at len(src), src has a NUL byte there. Tokens goes on past an [Illegal]
// token, where SQLite's parser stops and reads no more.
func Tokens(src string) []Token {
	var tokens []Token
	offset := 0
	for {
		kind, n := Next(src[offset:])
		if n == 0 {
			return tokens
		}
		tokens = append(tokens, Token{Kind: kind, Offset: offset, Len: n})
		offset += n
	}
}

// Character classes of the first byte of a token, as CC_ in tokenize.c.
const (
	ccX        = 0  // the letter 'x', or start of BLOB literal
	ccKywd0    = 1  // first letter of a keyword
	ccKywd     = 2  // alphabetics or '_'; usable in a keyword
	ccDigit    = 3  // digits
	ccDollar   = 4  // '$'
	ccVarAlpha = 5  // '@', '#', ':'; alphabetic SQL variables
	ccVarNum   = 6  // '?'; numeric SQL variables
	ccSpace    = 7  // space characters
	ccQuote    = 8  // '"', '\'', or '`'; string literals, quoted ids
	ccQuote2   = 9  // '['; [...] style quoted ids
	ccPipe     = 10 // '|'; bitwise OR or concatenate
	ccMinus    = 11 // '-'; minus or SQL-style comment
	ccLT       = 12 // '<'; part of < or <= or <>
	ccGT       = 13 // '>'; part of > or >=
	ccEQ       = 14 // '='; part of = or ==
	ccBang     = 15 // '!'; part of !=
	ccSlash    = 16 // '/'; / or c-style comment
	ccLP       = 17 // '('
	ccRP       = 18 // ')'
	ccSemi     = 19 // ';'
	ccPlus     = 20 // '+'
	ccStar     = 21 // '*'
	ccPercent  = 22 // '%'
	ccComma    = 23 // ','
	ccAnd      = 24 // '&'
	ccTilda    = 25 // '~'
	ccDot      = 26 // '.'
	ccID       = 27 // unicode characters usable in IDs
	ccIllegal  = 28 // illegal character
	ccNUL      = 29 // 0x00
	ccBOM      = 30 // first byte of UTF8 BOM: 0xEF 0xBB 0xBF
)

// aiClass of every byte, as in tokenize.c for SQLITE_ASCII.
var aiClass = [256]uint8{
	/*         x0  x1  x2  x3  x4  x5  x6  x7  x8  x9  xa  xb  xc  xd  xe  xf */
	/* 0x */ 29, 28, 28, 28, 28, 28, 28, 28, 28, 7, 7, 28, 7, 7, 28, 28,
	/* 1x */ 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28,
	/* 2x */ 7, 15, 8, 5, 4, 22, 24, 8, 17, 18, 21, 20, 23, 11, 26, 16,
	/* 3x */ 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 5, 19, 12, 14, 13, 6,
	/* 4x */ 5, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	/* 5x */ 1, 1, 1, 1, 1, 1, 1, 1, 0, 2, 2, 9, 28, 28, 28, 2,
	/* 6x */ 8, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	/* 7x */ 1, 1, 1, 1, 1, 1, 1, 1, 0, 2, 2, 28, 10, 28, 25, 28,
	/* 8x */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* 9x */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* Ax */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* Bx */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* Cx */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* Dx */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	/* Ex */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 30,
	/* Fx */ 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
}

// ctypeMap of every byte, as sqlite3CtypeMap in util.c (sqlite3.c 24242): 0x01 is space, 0x02 is an
// alphabetic, 0x04 a digit, 0x08 a hex digit, 0x20 a lower-case letter, 0x40 '$', '_', or a byte
// from 0x80 up, and 0x80 a quote.
var ctypeMap = [256]uint8{
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, /* 00..07    ........ */
	0x00, 0x01, 0x01, 0x01, 0x01, 0x01, 0x00, 0x00, /* 08..0f    ........ */
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, /* 10..17    ........ */
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, /* 18..1f    ........ */
	0x01, 0x00, 0x80, 0x00, 0x40, 0x00, 0x00, 0x80, /* 20..27     !"#$%&' */
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, /* 28..2f    ()*+,-./ */
	0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, 0x0c, /* 30..37    01234567 */
	0x0c, 0x0c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, /* 38..3f    89:;<=>? */

	0x00, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x0a, 0x02, /* 40..47    @ABCDEFG */
	0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, /* 48..4f    HIJKLMNO */
	0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, 0x02, /* 50..57    PQRSTUVW */
	0x02, 0x02, 0x02, 0x80, 0x00, 0x00, 0x00, 0x40, /* 58..5f    XYZ[\]^_ */
	0x80, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x2a, 0x22, /* 60..67    `abcdefg */
	0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, /* 68..6f    hijklmno */
	0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, /* 70..77    pqrstuvw */
	0x22, 0x22, 0x22, 0x00, 0x00, 0x00, 0x00, 0x00, /* 78..7f    xyz{|}~. */

	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* 80..87    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* 88..8f    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* 90..97    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* 98..9f    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* a0..a7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* a8..af    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* b0..b7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* b8..bf    ........ */

	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* c0..c7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* c8..cf    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* d0..d7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* d8..df    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* e0..e7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* e8..ef    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* f0..f7    ........ */
	0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, 0x40, /* f8..ff    ........ */
}

// isSpace is sqlite3Isspace: space, tab, line feed, vertical tab, form feed, and carriage return.
func isSpace(c byte) bool { return ctypeMap[c]&0x01 != 0 }

// isDigit is sqlite3Isdigit.
func isDigit(c byte) bool { return ctypeMap[c]&0x04 != 0 }

// isXDigit is sqlite3Isxdigit.
func isXDigit(c byte) bool { return ctypeMap[c]&0x08 != 0 }

// idChar is IdChar in tokenize.c: ASCII letters and digits, '_', '$', and every byte from 0x80 up.
func idChar(c byte) bool { return ctypeMap[c]&0x46 != 0 }

// digitSeparator is SQLITE_DIGIT_SEPARATOR.
const digitSeparator = '_'

// Next token at the start of z: its kind and its length in bytes. It is sqlite3GetToken, and z[i]
// below is z[i] there, with every byte at or past the end of z read as the NUL that ends SQLite's
// string. At the end of z, or at a NUL byte, it gives [Illegal] and length 0; every other token is
// at least one byte long.
func Next(z string) (Kind, int) {
	at := func(i int) byte {
		if i < len(z) {
			return z[i]
		}
		return 0
	}

	var i int
	switch aiClass[at(0)] {
	case ccSpace:
		for i = 1; isSpace(at(i)); i++ {
		}
		return Space, i

	case ccMinus:
		if at(1) == '-' {
			for i = 2; at(i) != 0 && at(i) != '\n'; i++ {
			}
			return Comment, i
		} else if at(1) == '>' {
			if at(2) == '>' {
				return Ptr, 3
			}
			return Ptr, 2
		}
		return Minus, 1

	case ccLP:
		return LP, 1

	case ccRP:
		return RP, 1

	case ccSemi:
		return Semi, 1

	case ccPlus:
		return Plus, 1

	case ccStar:
		return Star, 1

	case ccSlash:
		if at(1) != '*' || at(2) == 0 {
			return Slash, 1
		}
		// for(i=3, c=z[2]; (c!='*' || z[i]!='/') && (c=z[i])!=0; i++){}
		c := at(2)
		for i = 3; c != '*' || at(i) != '/'; i++ {
			if c = at(i); c == 0 {
				break
			}
		}
		if c != 0 {
			i++
		}
		return Comment, i

	case ccPercent:
		return Rem, 1

	case ccEQ:
		if at(1) == '=' {
			return EQ, 2
		}
		return EQ, 1

	case ccLT:
		switch at(1) {
		case '=':
			return LE, 2
		case '>':
			return NE, 2
		case '<':
			return LShift, 2
		default:
			return LT, 1
		}

	case ccGT:
		switch at(1) {
		case '=':
			return GE, 2
		case '>':
			return RShift, 2
		default:
			return GT, 1
		}

	case ccBang:
		if at(1) != '=' {
			return Illegal, 1
		}
		return NE, 2

	case ccPipe:
		if at(1) != '|' {
			return BitOr, 1
		}
		return Concat, 2

	case ccComma:
		return Comma, 1

	case ccAnd:
		return BitAnd, 1

	case ccTilda:
		return BitNot, 1

	case ccQuote:
		delim := at(0)
		var c byte
		for i = 1; ; i++ {
			if c = at(i); c == 0 {
				break
			}
			if c == delim {
				if at(i+1) == delim {
					i++
				} else {
					break
				}
			}
		}
		if c == '\'' {
			return String, i + 1
		} else if c != 0 {
			return ID, i + 1
		}
		return Illegal, i

	case ccDot:
		if !isDigit(at(1)) {
			return Dot, 1
		}
		// If the next character is a digit, this is a floating point number that begins with ".".
		fallthrough

	case ccDigit:
		kind := Integer
		if at(0) == '0' && (at(1) == 'x' || at(1) == 'X') && isXDigit(at(2)) {
			for i = 3; ; i++ {
				if !isXDigit(at(i)) {
					if at(i) != digitSeparator {
						break
					}
					kind = QNumber
				}
			}
		} else {
			for i = 0; ; i++ {
				if !isDigit(at(i)) {
					if at(i) != digitSeparator {
						break
					}
					kind = QNumber
				}
			}
			if at(i) == '.' {
				if kind == Integer {
					kind = Float
				}
				for i++; ; i++ {
					if !isDigit(at(i)) {
						if at(i) != digitSeparator {
							break
						}
						kind = QNumber
					}
				}
			}
			if (at(i) == 'e' || at(i) == 'E') &&
				(isDigit(at(i+1)) || ((at(i+1) == '+' || at(i+1) == '-') && isDigit(at(i+2)))) {
				if kind == Integer {
					kind = Float
				}
				for i += 2; ; i++ {
					if !isDigit(at(i)) {
						if at(i) != digitSeparator {
							break
						}
						kind = QNumber
					}
				}
			}
		}
		for idChar(at(i)) {
			kind = Illegal
			i++
		}
		return kind, i

	case ccQuote2:
		// for(i=1, c=z[0]; c!=']' && (c=z[i])!=0; i++){}
		c := at(0)
		for i = 1; c != ']'; i++ {
			if c = at(i); c == 0 {
				break
			}
		}
		if c == ']' {
			return ID, i
		}
		return Illegal, i

	case ccVarNum:
		for i = 1; isDigit(at(i)); i++ {
		}
		return Variable, i

	case ccDollar, ccVarAlpha:
		kind := Variable
		n := 0
		for i = 1; at(i) != 0; i++ {
			c := at(i)
			if idChar(c) {
				n++
			} else if c == '(' && n > 0 {
				for {
					i++
					if c = at(i); c == 0 || isSpace(c) || c == ')' {
						break
					}
				}
				if c == ')' {
					i++
				} else {
					kind = Illegal
				}
				break
			} else if c == ':' && at(i+1) == ':' {
				i++
			} else {
				break
			}
		}
		if n == 0 {
			kind = Illegal
		}
		return kind, i

	case ccKywd0:
		if aiClass[at(1)] > ccKywd {
			i = 1
			break
		}
		for i = 2; aiClass[at(i)] <= ccKywd; i++ {
		}
		if idChar(at(i)) {
			// This token started out using characters that can appear in keywords, but z[i] is a
			// character not allowed within keywords, so this must be an identifier instead.
			i++
			break
		}
		return keywordCode(z[:i]), i

	case ccX:
		if at(1) == '\'' {
			kind := Blob
			for i = 2; isXDigit(at(i)); i++ {
			}
			if at(i) != '\'' || i%2 != 0 {
				kind = Illegal
				for at(i) != 0 && at(i) != '\'' {
					i++
				}
			}
			if at(i) != 0 {
				i++
			}
			return kind, i
		}
		// If it is not a BLOB literal, then it must be an ID, since no SQL keywords start with the
		// letter 'x'.
		i = 1

	case ccKywd, ccID:
		i = 1

	case ccBOM:
		if at(1) == 0xbb && at(2) == 0xbf {
			return Space, 3
		}
		i = 1

	case ccNUL:
		return Illegal, 0

	default:
		return Illegal, 1
	}

	for idChar(at(i)) {
		i++
	}
	return ID, i
}

// keywordCode of the word w, which is made of ASCII letters and '_' only, as keywordCode in
// keywordhash.h finds it: the kind of the keyword w is, in any case of its letters, or [ID]. For
// such words, upper-casing the letters is the same as the comparison there, which clears the bit
// 0x20 of each byte.
func keywordCode(w string) Kind {
	// The longest keyword, CURRENT_TIMESTAMP, has 17 letters.
	var upper [17]byte
	if len(w) > len(upper) {
		return ID
	}
	for i := range len(w) {
		c := w[i]
		if 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		upper[i] = c
	}
	if kind, ok := keywords[string(upper[:len(w)])]; ok {
		return kind
	}
	return ID
}
