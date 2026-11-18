package rss

import (
	"bufio"
	"io"
	"unicode/utf8"
)

// ISO-8859-1 support.
//
// The decoder implements both io.Reader and io.ByteReader: encoding/xml only
// uses ReadByte today, but returning an error from Read made the reader
// unusable for anything else, and the unchecked type assertion on the input
// panicked whenever the caller passed a plain io.Reader.

type charsetISO88591er struct {
	r   io.ByteReader
	buf []byte // pending UTF-8 bytes of a decoded rune
}

func newCharsetISO88591(r io.Reader) *charsetISO88591er {
	br, ok := r.(io.ByteReader)
	if !ok {
		br = bufio.NewReader(r)
	}
	return &charsetISO88591er{r: br, buf: make([]byte, 0, utf8.UTFMax)}
}

func (cs *charsetISO88591er) ReadByte() (byte, error) {
	// http://unicode.org/Public/MAPPINGS/ISO8859/8859-1.TXT
	// Date: 1999 July 27; Last modified: 27-Feb-2001 05:08
	for {
		if len(cs.buf) > 0 {
			b := cs.buf[0]
			cs.buf = cs.buf[1:]
			return b, nil
		}
		b, err := cs.r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b < utf8.RuneSelf {
			return b, nil
		}
		// Latin-1 maps every byte 1:1 onto the same Unicode code point.
		cs.buf = utf8.AppendRune(cs.buf[:0], rune(b))
	}
}

func (cs *charsetISO88591er) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n int
	for n < len(p) {
		b, err := cs.ReadByte()
		if err != nil {
			if err == io.EOF && n > 0 {
				return n, nil
			}
			return n, err
		}
		p[n] = b
		n++
	}
	return n, nil
}

func isCharsetISO88591(charset string) bool {
	// http://www.iana.org/assignments/character-sets
	// (last updated 2010-11-04)
	names := []string{
		// Name
		"ISO_8859-1:1987",
		// Alias (preferred MIME name)
		"ISO-8859-1",
		// Aliases
		"iso-ir-100",
		"ISO_8859-1",
		"latin1",
		"l1",
		"IBM819",
		"CP819",
		"csISOLatin1",
		"us-ascii",
	}
	return isCharset(charset, names)
}
