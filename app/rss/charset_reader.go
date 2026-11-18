package rss

import (
	"errors"
	"io"
	"strings"

	"github.com/axgle/mahonia"
)

// charsetReader converts a feed body that declares a non UTF-8 encoding into
// UTF-8. It must accept any io.Reader: encoding/xml hands us its internal
// reader, which is a io.ByteReader today but that is an implementation detail.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch {
	case isCharsetUTF8(charset):
		return input, nil
	case isCharsetISO88591(charset):
		return newCharsetISO88591(input), nil
	default:
		if decoder := mahonia.NewDecoder(charset); decoder != nil {
			return decoder.NewReader(input), nil
		}
	}

	return nil, errors.New("rss: unsupported charset: " + charset)
}

func isCharset(charset string, names []string) bool {
	charset = strings.ToLower(strings.TrimSpace(charset))
	for _, n := range names {
		if charset == strings.ToLower(n) {
			return true
		}
	}
	return false
}

func isCharsetUTF8(charset string) bool {
	names := []string{
		"UTF-8",
		"UTF8",
		"utf-8",
		// Default when the XML declaration omits the encoding.
		"",
	}
	return isCharset(charset, names)
}
