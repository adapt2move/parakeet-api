package api

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// Limits of python-multipart's MultipartParser.
const (
	maxBoundaryLength = 256
	maxPartHeaders    = 8
	maxPartHeaderSize = 4096 + 128
)

// errTruncated is a body that ended inside a part. python-multipart never finished such a part,
// so Starlette's form lacked it; it is not an error.
var errTruncated = errors.New("multipart body ended inside a part")

// readMultipart parses a multipart/form-data body the way Starlette's MultiPartParser drove
// python-multipart's MultipartParser. Every MultipartParseError of that parser was a ValueError,
// which the API answered with errInvalidOptions. The "file" part streams into a reserved upload.
func (a *API) readMultipart(body io.Reader, boundary []byte, f *form) error {
	if len(boundary) > maxBoundaryLength {
		return errInvalidOptions
	}
	p := &multipartParser{br: bufio.NewReaderSize(body, 64*1024), delimiter: append([]byte("\r\n--"), boundary...)}
	more, err := p.start()
	files, fields := 0, 0
	for more && err == nil {
		var disposition string
		if disposition, more, err = p.headers(); !more || err != nil {
			break
		}
		_, options := parseOptionsHeader(disposition)
		rawName, ok := options["name"]
		if !ok {
			return errMissingName
		}
		name := userSafeDecode(latin1Bytes(rawName))
		part := &partReader{p: p}
		if _, isFile := options["filename"]; !isFile {
			fields++
			if fields > maxFormFields {
				return errTooManyFields
			}
			data, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if len(data) > maxFieldBytes {
				return errPartTooLarge
			}
			if err == errTruncated {
				return nil
			}
			if err != nil {
				return err
			}
			f.values = append(f.values, formValue{name: name, value: userSafeDecode(data)})
		} else {
			files++
			if files > maxFormFiles {
				return errTooManyFiles
			}
			if name == "file" {
				err = a.receiveFile(part, f)
			} else {
				_, err = io.Copy(io.Discard, part)
			}
			if err == errTruncated {
				if f.upload != "" {
					a.store.DiscardUpload(f.upload)
					f.upload, f.size, f.saveErr = "", 0, nil
				}
				return nil
			}
			if err != nil {
				return err
			}
			f.values = append(f.values, formValue{name: name, file: true})
		}
		more = !part.last
	}
	return err
}

type multipartParser struct {
	br        *bufio.Reader
	delimiter []byte // CRLF, "--" and the boundary
}

// endOfBody turns the end of the body into the end of the form; other read errors stay.
func endOfBody(err error) error {
	if err == io.EOF {
		return nil
	}
	return err
}

// start reads up to the first part. It reports false for a form that has no part: an empty
// body, a body that ends early, or an immediate closing delimiter.
func (p *multipartParser) start() (bool, error) {
	br := p.br
	c, err := br.ReadByte()
	if err != nil {
		return false, endOfBody(err)
	}
	if c == '\r' || c == '\n' {
		// python-multipart jumped from a leading line break to the next hyphen of the chunk it
		// was given; the read buffer stands in for uvicorn's chunk.
		for skipped := 1; c != '-'; skipped++ {
			if skipped >= br.Size() {
				return false, errInvalidOptions
			}
			if c, err = br.ReadByte(); err != nil {
				return false, endOfBody(err)
			}
		}
	}
	want := p.delimiter[2:]
	for i := range want {
		if i > 0 {
			if c, err = br.ReadByte(); err != nil {
				return false, endOfBody(err)
			}
		}
		if c != want[i] {
			return false, errInvalidOptions
		}
	}
	if c, err = br.ReadByte(); err != nil {
		return false, endOfBody(err)
	}
	var next byte
	switch c {
	case '-':
		next = '-'
	case '\r':
		next = '\n'
	default:
		return false, errInvalidOptions
	}
	if c, err = br.ReadByte(); err != nil {
		return false, endOfBody(err)
	}
	if c != next {
		return false, errInvalidOptions
	}
	return next == '\n', nil
}

// headers reads the header block of a part and returns its last Content-Disposition value,
// decoded as Latin-1 like Starlette did. ok is false when the body ended first.
func (p *multipartParser) headers() (disposition string, ok bool, err error) {
	br := p.br
	read := func() (byte, bool) {
		c, e := br.ReadByte()
		if e != nil {
			err = endOfBody(e)
			return 0, false
		}
		return c, true
	}
	for count := 1; ; count++ {
		c, more := read()
		if !more {
			return "", false, err
		}
		if c == '\r' {
			if c, more = read(); !more {
				return "", false, err
			}
			if c != '\n' {
				return "", false, errInvalidOptions
			}
			return disposition, true, nil
		}
		if count > maxPartHeaders {
			return "", false, errInvalidOptions
		}
		size := 0
		var name []byte
		for c != ':' {
			if size++; size > maxPartHeaderSize || !isTokenChar(c) {
				return "", false, errInvalidOptions
			}
			name = append(name, c)
			if c, more = read(); !more {
				return "", false, err
			}
		}
		if size++; size > maxPartHeaderSize || len(name) == 0 {
			return "", false, errInvalidOptions
		}
		for {
			if c, more = read(); !more {
				return "", false, err
			}
			if c != ' ' {
				break
			}
			if size++; size > maxPartHeaderSize {
				return "", false, errInvalidOptions
			}
		}
		var value []byte
		for c != '\r' {
			if size++; size > maxPartHeaderSize {
				return "", false, errInvalidOptions
			}
			value = append(value, c)
			if c, more = read(); !more {
				return "", false, err
			}
		}
		if c, more = read(); !more {
			return "", false, err
		}
		if c != '\n' {
			return "", false, errInvalidOptions
		}
		if strings.EqualFold(string(name), "content-disposition") {
			disposition = latin1String(value)
		}
	}
}

// isTokenChar reports an RFC 7230 token character, the only bytes python-multipart allowed in
// a header name.
func isTokenChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// partReader yields the data of the current part. python-multipart took the delimiter as the
// end of the part only when CRLF (another part follows) or "--" (the last part) came next;
// anything else after it was part data.
type partReader struct {
	p    *multipartParser
	done bool
	last bool // the part ended with the closing delimiter
	err  error
}

func (r *partReader) Read(b []byte) (int, error) {
	switch {
	case r.done:
		return 0, io.EOF
	case r.err != nil:
		return 0, r.err
	case len(b) == 0:
		return 0, nil
	}
	br, delimiter := r.p.br, r.p.delimiter
	need := len(delimiter) + 2
	_, err := br.Peek(need)
	data, _ := br.Peek(br.Buffered())
	if err != nil {
		if err != io.EOF {
			r.err = err
			return 0, err
		}
		// The body ended inside the part. Data was emitted up to a possible start of the
		// delimiter; the part itself never ended.
		safe := len(data) - pendingDelimiter(data, delimiter)
		if safe == 0 {
			r.err = errTruncated
			return 0, r.err
		}
		n := copy(b, data[:safe])
		br.Discard(n)
		return n, nil
	}
	safe := len(data) - partialPrefix(data, delimiter)
	for from := 0; ; {
		i := bytes.Index(data[from:], delimiter)
		if i < 0 {
			break
		}
		pos := from + i
		if pos+need > len(data) {
			safe = pos // undecided until more data arrives
			break
		}
		if suffix := string(data[pos+len(delimiter) : pos+need]); suffix == "\r\n" || suffix == "--" {
			if pos == 0 {
				br.Discard(need)
				r.done, r.last = true, suffix == "--"
				return 0, io.EOF
			}
			safe = pos
			break
		}
		from = pos + 1
	}
	n := copy(b, data[:safe])
	br.Discard(n)
	return n, nil
}

// partialPrefix is the length of the longest suffix of data that is a proper prefix of the
// delimiter. The delimiter starts with its only CR, so only the last CR can start one.
func partialPrefix(data, delimiter []byte) int {
	from := max(0, len(data)-len(delimiter)+1)
	k := bytes.LastIndexByte(data[from:], '\r')
	if k < 0 || !bytes.HasPrefix(delimiter, data[from+k:]) {
		return 0
	}
	return len(data) - from - k
}

// pendingDelimiter is the length of the tail of data that could still become a delimiter
// followed by CRLF or "--": a prefix of the delimiter, or the delimiter and its next byte.
func pendingDelimiter(data, delimiter []byte) int {
	if n := len(data); n > len(delimiter) {
		tail := data[n-len(delimiter)-1:]
		if bytes.HasPrefix(tail, delimiter) && (tail[len(delimiter)] == '\r' || tail[len(delimiter)] == '-') {
			return len(tail)
		}
	}
	if n := len(data); n >= len(delimiter) && bytes.Equal(data[n-len(delimiter):], delimiter) {
		return len(delimiter)
	}
	return partialPrefix(data, delimiter)
}

// latin1String decodes Latin-1 bytes, as Starlette decoded header values.
func latin1String(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// latin1Bytes encodes a string decoded by latin1String back to its bytes.
func latin1Bytes(s string) []byte {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	return b
}

// userSafeDecode is Starlette's _user_safe_decode for UTF-8: invalid UTF-8 decodes as Latin-1.
func userSafeDecode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return latin1String(b)
}
