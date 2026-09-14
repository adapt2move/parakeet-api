package formats

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The Python API decoded request bodies with json.loads and validated them with pydantic in lax
// mode. This file reproduces the json.loads half: its byte-encoding detection, its grammar
// (NaN/Infinity literals, strict control characters, surrogate escapes), its 4300-digit integer
// limit and the order in which those errors surface.

var (
	// ErrBodyParse matches FastAPI's 400 {"detail":"There was an error parsing the body"}: the
	// body could not be decoded as text, held an integer literal over 4300 digits, or nested
	// deeper than CPython's recursion limit allowed.
	ErrBodyParse = errors.New("formats: body cannot be parsed")
	// ErrInvalid matches the 422 "Invalid or unsupported request fields" response: malformed
	// JSON, a non-JSON content type, or a pydantic validation failure.
	ErrInvalid = errors.New("formats: invalid or unsupported request fields")
)

// maxDepth is the deepest container nesting json.loads survived under the Python API before
// raising RecursionError under uvicorn (the exact limit depended on the frames already in use).
const maxDepth = 961

// maxIntDigits is CPython's default int_max_str_digits.
const maxIntDigits = 4300

type kind uint8

const (
	kindNull kind = iota
	kindBool
	kindInt
	kindFloat
	kindString
	kindArray
	kindObject
)

// value is a decoded JSON value. s holds a string's contents or a number's literal; NaN and the
// infinities are kindFloat with the literal "NaN", "Infinity" or "-Infinity". items holds array
// elements or object members in order, members carrying their name in key.
type value struct {
	kind  kind
	b     bool
	s     string
	key   string
	items []value
}

// JSONContentType reports whether FastAPI would parse a body with this Content-Type header as
// JSON: application/json or application/*+json, parameters ignored.
func JSONContentType(header string) bool {
	if header == "" {
		return false
	}
	// email.message: split off parameters, strip, lower, and require exactly one slash.
	header, _, _ = strings.Cut(header, ";")
	ctype := asciiLower(latin1Strip(header))
	if strings.Count(ctype, "/") != 1 {
		return false
	}
	main, sub, _ := strings.Cut(ctype, "/")
	return main == "application" && (sub == "json" || strings.HasSuffix(sub, "+json"))
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// latin1Strip strips what Python's str.strip removes from a latin-1 decoded header value.
func latin1Strip(s string) string {
	space := func(c byte) bool {
		return c == ' ' || (c >= '\t' && c <= '\r') || (c >= 0x1c && c <= 0x1f) || c == 0x85 || c == 0xa0
	}
	for len(s) > 0 && space(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && space(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// decodeRequest reproduces FastAPI's body handling for a pydantic model parameter.
func decodeRequest(contentType string, body []byte) (value, error) {
	if len(body) == 0 || !JSONContentType(contentType) {
		return value{}, ErrInvalid
	}
	text, surrogates, err := decodeText(body)
	if err != nil {
		return value{}, err
	}
	return parseText(text, surrogates)
}

// parseText parses UTF-8 text. Any lone surrogate, raw or escaped, passes json.loads but fails
// pydantic's string conversion, so it becomes ErrInvalid once parsing succeeds.
func parseText(text []byte, surrogates bool) (value, error) {
	p := parser{src: text}
	p.skip()
	v, err := p.value(0)
	if err != nil {
		return value{}, err
	}
	p.skip()
	if p.pos != len(p.src) {
		return value{}, ErrInvalid
	}
	if surrogates || p.surrogates {
		return value{}, ErrInvalid
	}
	return v, nil
}

// decodeText mirrors json.detect_encoding and bytes.decode(encoding, "surrogatepass"). Lone
// surrogates are replaced by U+FFFD and reported, since they always end in a 422.
func decodeText(b []byte) ([]byte, bool, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		return decodeUTF32(b[4:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		return decodeUTF32(b[4:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		return decodeUTF16(b[2:], binary.BigEndian)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		return decodeUTF16(b[2:], binary.LittleEndian)
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		return decodeUTF8(b[3:])
	}
	if len(b) >= 4 {
		if b[0] == 0 {
			if b[1] != 0 {
				return decodeUTF16(b, binary.BigEndian)
			}
			return decodeUTF32(b, binary.BigEndian)
		}
		if b[1] == 0 {
			if b[2] != 0 || b[3] != 0 {
				return decodeUTF16(b, binary.LittleEndian)
			}
			return decodeUTF32(b, binary.LittleEndian)
		}
	} else if len(b) == 2 {
		if b[0] == 0 {
			return decodeUTF16(b, binary.BigEndian)
		}
		if b[1] == 0 {
			return decodeUTF16(b, binary.LittleEndian)
		}
	}
	return decodeUTF8(b)
}

func decodeUTF8(b []byte) ([]byte, bool, error) {
	if utf8.Valid(b) {
		return b, false, nil
	}
	out := make([]byte, 0, len(b))
	surrogates := false
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			// surrogatepass accepts the three-byte encodings of U+D800..U+DFFF.
			if b[i] == 0xed && i+2 < len(b) && b[i+1] >= 0xa0 && b[i+1] <= 0xbf && b[i+2] >= 0x80 && b[i+2] <= 0xbf {
				out = utf8.AppendRune(out, utf8.RuneError)
				surrogates = true
				i += 3
				continue
			}
			return nil, false, ErrBodyParse
		}
		out = append(out, b[i:i+size]...)
		i += size
	}
	return out, surrogates, nil
}

func decodeUTF16(b []byte, order binary.ByteOrder) ([]byte, bool, error) {
	if len(b)%2 != 0 {
		return nil, false, ErrBodyParse
	}
	out := make([]byte, 0, len(b))
	surrogates := false
	for i := 0; i < len(b); i += 2 {
		r := rune(order.Uint16(b[i:]))
		if r >= 0xd800 && r <= 0xdbff && i+4 <= len(b) {
			if low := rune(order.Uint16(b[i+2:])); low >= 0xdc00 && low <= 0xdfff {
				out = utf8.AppendRune(out, 0x10000+(r-0xd800)<<10+(low-0xdc00))
				i += 2
				continue
			}
		}
		if r >= 0xd800 && r <= 0xdfff {
			r, surrogates = utf8.RuneError, true
		}
		out = utf8.AppendRune(out, r)
	}
	return out, surrogates, nil
}

func decodeUTF32(b []byte, order binary.ByteOrder) ([]byte, bool, error) {
	if len(b)%4 != 0 {
		return nil, false, ErrBodyParse
	}
	out := make([]byte, 0, len(b))
	surrogates := false
	for i := 0; i < len(b); i += 4 {
		c := order.Uint32(b[i:])
		if c > 0x10ffff {
			return nil, false, ErrBodyParse
		}
		r := rune(c)
		if r >= 0xd800 && r <= 0xdfff {
			r, surrogates = utf8.RuneError, true
		}
		out = utf8.AppendRune(out, r)
	}
	return out, surrogates, nil
}

type parser struct {
	src        []byte
	pos        int
	surrogates bool
	// stack collects container elements while they are parsed, so each container allocates
	// exactly once when it closes.
	stack []value
}

func (p *parser) close(v *value, mark int) {
	v.items = make([]value, len(p.stack)-mark)
	copy(v.items, p.stack[mark:])
	clear(p.stack[mark:])
	p.stack = p.stack[:mark]
}

func (p *parser) skip() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) literal(word string) bool {
	if bytes.HasPrefix(p.src[p.pos:], []byte(word)) {
		p.pos += len(word)
		return true
	}
	return false
}

func (p *parser) value(depth int) (value, error) {
	if p.pos >= len(p.src) {
		return value{}, ErrInvalid
	}
	switch c := p.src[p.pos]; {
	case c == '"':
		p.pos++
		s, err := p.str(false)
		if err != nil {
			return value{}, err
		}
		return value{kind: kindString, s: s}, nil
	case c == '{':
		if depth+1 > maxDepth {
			return value{}, ErrBodyParse
		}
		p.pos++
		return p.object(depth + 1)
	case c == '[':
		if depth+1 > maxDepth {
			return value{}, ErrBodyParse
		}
		p.pos++
		return p.array(depth + 1)
	case c == 'n' && p.literal("null"):
		return value{kind: kindNull}, nil
	case c == 't' && p.literal("true"):
		return value{kind: kindBool, b: true}, nil
	case c == 'f' && p.literal("false"):
		return value{kind: kindBool}, nil
	case c == 'N' && p.literal("NaN"):
		return value{kind: kindFloat, s: "NaN"}, nil
	case c == 'I' && p.literal("Infinity"):
		return value{kind: kindFloat, s: "Infinity"}, nil
	case c == '-' && p.literal("-Infinity"):
		return value{kind: kindFloat, s: "-Infinity"}, nil
	}
	return p.number()
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// number follows json.scanner's NUMBER_RE: -?(0|[1-9]\d*)(\.\d+)?([eE][-+]?\d+)?
func (p *parser) number() (value, error) {
	start, i := p.pos, p.pos
	if i < len(p.src) && p.src[i] == '-' {
		i++
	}
	switch {
	case i < len(p.src) && p.src[i] >= '1' && p.src[i] <= '9':
		for i < len(p.src) && isDigit(p.src[i]) {
			i++
		}
	case i < len(p.src) && p.src[i] == '0':
		i++
	default:
		return value{}, ErrInvalid
	}
	float := false
	if i+1 < len(p.src) && p.src[i] == '.' && isDigit(p.src[i+1]) {
		float = true
		i += 2
		for i < len(p.src) && isDigit(p.src[i]) {
			i++
		}
	}
	if i < len(p.src) && (p.src[i] == 'e' || p.src[i] == 'E') {
		j := i + 1
		if j < len(p.src) && (p.src[j] == '-' || p.src[j] == '+') {
			j++
		}
		k := j
		for k < len(p.src) && isDigit(p.src[k]) {
			k++
		}
		if k > j {
			float, i = true, k
		}
	}
	p.pos = i
	lit := string(p.src[start:i])
	if float {
		return value{kind: kindFloat, s: lit}, nil
	}
	digits := len(lit)
	if lit[0] == '-' {
		digits--
	}
	if digits > maxIntDigits {
		return value{}, ErrBodyParse
	}
	return value{kind: kindInt, s: lit}, nil
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		r <<= 4
		switch {
		case '0' <= c && c <= '9':
			r |= rune(c - '0')
		case 'a' <= c && c <= 'f':
			r |= rune(c-'a') + 10
		case 'A' <= c && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}

// knownKeys are member names shared as constants instead of allocated per object.
var knownKeys = []string{"text", "start", "end", "confidence", "speaker", "channel", "words", "audio_duration_ms", "chunks", "seam_fallbacks", "result", "error", "retry", "audio_url", "language_code"}

// str follows the C scanstring in strict mode; the opening quote is consumed.
func (p *parser) str(key bool) (string, error) {
	var out []byte
	chunk := p.pos
	for {
		if p.pos >= len(p.src) {
			return "", ErrInvalid
		}
		c := p.src[p.pos]
		if c == '"' {
			if out == nil {
				raw := p.src[chunk:p.pos]
				p.pos++
				if key {
					for _, known := range knownKeys {
						if string(raw) == known {
							return known, nil
						}
					}
				}
				return string(raw), nil
			}
			out = append(out, p.src[chunk:p.pos]...)
			p.pos++
			return string(out), nil
		}
		if c < 0x20 {
			return "", ErrInvalid
		}
		if c != '\\' {
			p.pos++
			continue
		}
		out = append(out, p.src[chunk:p.pos]...)
		p.pos++
		if p.pos >= len(p.src) {
			return "", ErrInvalid
		}
		esc := p.src[p.pos]
		p.pos++
		switch esc {
		case '"', '\\', '/':
			out = append(out, esc)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(p.src[p.pos:])
			if !ok {
				return "", ErrInvalid
			}
			p.pos += 4
			// A high surrogate joins a directly following \u low surrogate. The C scanner only
			// looks ahead when at least one character follows the second escape.
			if r >= 0xd800 && r <= 0xdbff && p.pos+6 < len(p.src) && p.src[p.pos] == '\\' && p.src[p.pos+1] == 'u' {
				low, ok := hex4(p.src[p.pos+2:])
				if !ok {
					return "", ErrInvalid
				}
				if low >= 0xdc00 && low <= 0xdfff {
					r = 0x10000 + (r-0xd800)<<10 + (low - 0xdc00)
					p.pos += 6
				}
			}
			if r >= 0xd800 && r <= 0xdfff {
				r, p.surrogates = utf8.RuneError, true
			}
			out = utf8.AppendRune(out, r)
		default:
			return "", ErrInvalid
		}
		chunk = p.pos
	}
}

func (p *parser) object(depth int) (value, error) {
	v, mark := value{kind: kindObject}, len(p.stack)
	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return v, nil
	}
	for {
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return value{}, ErrInvalid
		}
		p.pos++
		key, err := p.str(true)
		if err != nil {
			return value{}, err
		}
		p.skip()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return value{}, ErrInvalid
		}
		p.pos++
		p.skip()
		item, err := p.value(depth)
		if err != nil {
			return value{}, err
		}
		item.key = key
		p.stack = append(p.stack, item)
		p.skip()
		if p.pos < len(p.src) && p.src[p.pos] == '}' {
			p.pos++
			p.close(&v, mark)
			return v, nil
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ',' {
			return value{}, ErrInvalid
		}
		p.pos++
		p.skip()
	}
}

func (p *parser) array(depth int) (value, error) {
	v, mark := value{kind: kindArray}, len(p.stack)
	p.skip()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return v, nil
	}
	for {
		item, err := p.value(depth)
		if err != nil {
			return value{}, err
		}
		p.stack = append(p.stack, item)
		p.skip()
		if p.pos < len(p.src) && p.src[p.pos] == ']' {
			p.pos++
			p.close(&v, mark)
			return v, nil
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ',' {
			return value{}, ErrInvalid
		}
		p.pos++
		p.skip()
	}
}

// AppendString appends s as json.dumps(ensure_ascii=False) writes it: only quotes, backslashes
// and control characters are escaped. s must be valid UTF-8.
func AppendString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
		}
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// AppendFloat appends f as Python's float repr, which json.dumps uses: the shortest round-trip
// digits, a ".0" on integral values, and exponent notation below 1e-4 or from 1e16 up.
func AppendFloat(dst []byte, f float64) []byte {
	// Never produced for validated data; json.dumps(allow_nan=True) spells them this way.
	switch {
	case f != f:
		return append(dst, "NaN"...)
	case f > 1.7976931348623157e308:
		return append(dst, "Infinity"...)
	case f < -1.7976931348623157e308:
		return append(dst, "-Infinity"...)
	}
	var buf [32]byte
	e := strconv.AppendFloat(buf[:0], f, 'e', -1, 64)
	if e[0] == '-' {
		dst = append(dst, '-')
		e = e[1:]
	}
	mark := bytes.IndexByte(e, 'e')
	mantissa, exponent := e[:mark], e[mark+1:]
	// The mantissa is "d" or "d.ddd"; digits drops its point.
	var digitBuf [24]byte
	digits := append(digitBuf[:0], mantissa[0])
	if len(mantissa) > 2 {
		digits = append(digits, mantissa[2:]...)
	}
	exp := 0
	for _, c := range exponent[1:] {
		exp = exp*10 + int(c-'0')
	}
	if exponent[0] == '-' {
		exp = -exp
	}
	decpt := exp + 1
	switch {
	case decpt <= -4 || decpt > 16:
		return append(dst, e...)
	case decpt <= 0:
		dst = append(dst, "0."...)
		dst = appendZeros(dst, -decpt)
		return append(dst, digits...)
	case decpt >= len(digits):
		dst = append(dst, digits...)
		dst = appendZeros(dst, decpt-len(digits))
		return append(dst, ".0"...)
	default:
		dst = append(dst, digits[:decpt]...)
		dst = append(dst, '.')
		return append(dst, digits[decpt:]...)
	}
}

func appendZeros(dst []byte, n int) []byte {
	for range n {
		dst = append(dst, '0')
	}
	return dst
}
