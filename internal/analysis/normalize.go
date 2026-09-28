package analysis

import (
	"strings"
	"unicode"
)

// SongKey reduces an artist/title pair to the song it names, so the same
// song scrobbled by two apps with different metadata ("Song - Remastered
// 2011" from Spotify, "Song (Official Video)" from a YouTube scrobbler)
// compares equal. It only has to be good enough to match copies of one play
// seconds apart, so it errs on the side of merging.
func SongKey(artist, title string) string {
	return normArtist(artist) + "\x00" + normTitle(title)
}

func normTitle(s string) string {
	s = strings.ToLower(s)
	s = stripBracketed(s)
	if i := strings.Index(s, " - "); i > 0 {
		s = s[:i]
	}
	for _, sep := range []string{" feat. ", " feat ", " ft. ", " ft ", " featuring "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	return alnum(s)
}

func normArtist(s string) string {
	s = strings.ToLower(s)
	for _, sep := range []string{" feat. ", " feat ", " ft. ", " ft ", " featuring ", " & ", ", ", " x ", " and "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	return alnum(s)
}

func stripBracketed(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	if out := strings.TrimSpace(b.String()); out != "" {
		return out
	}
	return s
}

func alnum(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return strings.TrimSpace(s)
	}
	return b.String()
}
