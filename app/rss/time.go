package rss

import (
	"fmt"
	"strings"
	"time"
)

// zoneOffsets maps the abbreviation to its offset in hours. North American
// timezones are what RSS feeds overwhelmingly use (RFC 822 examples name them);
// anything else is left for the numeric offsets.
var zoneOffsets = map[string]int{
	"EST": -5, "EDT": -4, "CST": -6, "CDT": -5,
	"MST": -7, "MDT": -6, "PST": -8, "PDT": -7,
	"AKST": -9, "AKDT": -8, "HST": -10, "AST": -4,
	"GMT": 0, "UTC": 0, "UT": 0, "Z": 0,
	"CET": 1, "CEST": 2, "MET": 1, "MEST": 2,
	"WET": 0, "WEST": 1, "EET": 2, "EEST": 3,
	"MSK": 3, "IST": 5, "JST": 9, "KST": 9, "AEST": 10, "AEDT": 11, "NZST": 12, "NZDT": 13,
}

// normalizeZoneAbbreviation rewrites a trailing abbreviation into +hhmm so the
// standard layouts pick it up. A numeric offset in the string means there is
// nothing to guess and the input is returned untouched.
func normalizeZoneAbbreviation(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	last := fields[len(fields)-1]
	if strings.HasPrefix(last, "+") || strings.HasPrefix(last, "-") {
		return s // already an explicit offset
	}
	offset, ok := zoneOffsets[strings.ToUpper(last)]
	if !ok {
		return s
	}
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	fields[len(fields)-1] = fmt.Sprintf("%s%02d00", sign, offset)
	return strings.Join(fields, " ")
}

func parseTime(s string) (time.Time, error) {
	formats := []string{
		"Mon, _2 Jan 2006 15:04:05 MST",
		"Mon, _2 Jan 2006 15:04:05 Z",
		"Mon, _2 Jan 2006 15:04:05 UT",
		"Mon, _2 Jan 2006 15:04 MST",
		"Mon, _2 Jan 2006 15:04:05 -0700",
		"Mon, _2 Jan 2006 15:04:05",
		"_2 Jan 2006 15:04:05 -0700",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
		time.ANSIC,
		time.UnixDate,
		time.RubyDate,
		time.RFC822,
		time.RFC822Z,
		time.RFC850,
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
		time.RFC3339Nano,
	}

	s = strings.TrimSpace(s)

	// RFC 822 allows a zone abbreviation, and Go has no table for them: "MST"
	// parses happily with offset zero, which puts the item five hours off and -
	// because the fetcher clamps future dates - silently rewrites its publication
	// time. Normalising the handful of abbreviations feeds actually use turns the
	// abbreviation into a numeric offset.
	s = normalizeZoneAbbreviation(s)

	var e error
	var t time.Time

	for _, format := range formats {
		t, e = time.Parse(format, s)
		if e == nil {
			return t, e
		}
	}

	return time.Time{}, e
}
