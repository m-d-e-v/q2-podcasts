package library

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"

	"q2-podcasts/internal/feed"
	"q2-podcasts/internal/q2spot"
)

// Paths on the card stay well inside the firmware's: Q2 Pod's songs_open copies a local row's path
// into 256 bytes, so /mnt/mmc/Podcasts/ (18), a folder of at most MaxFolder and a file of at most
// MaxFile leave room to spare.
const (
	MaxFolder = 64
	MaxFile   = 150
)

// id is a short, stable hex id: a show's from its feed's URL, an episode's from its guid.
func id(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:5])
}

// KeyFor is the key a show subscribed from this URL gets.
func KeyFor(url string) string { return id(url) }

// EpisodeID is an episode's id within its show.
func EpisodeID(guid string) string { return id(guid) }

var reserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

// Safe is s as a FAT name: the characters FAT refuses replaced, no leading dot (a hidden file) or
// space, no trailing dot or space, at most n bytes, and never empty or a DOS device name.
func Safe(s string, n int) string {
	s = strings.NewReplacer(": ", " - ", ":", "-", "/", "-", "\\", "-", "|", "-", "\"", "'",
		"?", "", "*", "", "<", "", ">", "").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = q2spot.Cut(s, n)
	s = strings.TrimRight(strings.TrimLeft(s, ". "), ". ")
	up := strings.ToUpper(s)
	if s == "" {
		s = "Untitled"
	} else if reserved[up] || len(up) == 4 && (strings.HasPrefix(up, "COM") || strings.HasPrefix(up, "LPT")) &&
		up[3] >= '0' && up[3] <= '9' {
		s = "_" + s
	}
	return s
}

// fileNames names a show's episodes on the card: "2024-10-07 - Title.mp3", so a folder sorts oldest
// first and Q2 Pod plays on into the next newer episode. A name once given (in old) is kept, so a
// file on the card is never lost to a newcomer. A newcomer whose name is taken, or that another
// newcomer shares, gets " [<id>]" before the extension, which Q2 Pod's song lists leave off.
func fileNames(eps []feed.Episode, old map[string]string) map[string]string {
	names := make(map[string]string, len(eps))
	taken := map[string]bool{}
	for _, e := range eps {
		eid := EpisodeID(e.GUID)
		if n, ok := old[eid]; ok {
			names[eid] = n
			taken[strings.ToLower(n)] = true
		}
	}
	count := map[string]int{}
	base := make([]string, len(eps)) // "" for an episode already named
	for i, e := range eps {
		if _, ok := names[EpisodeID(e.GUID)]; ok {
			continue
		}
		ext := feed.Ext(e.Type, e.URL)
		prefix := ""
		if !e.Date.IsZero() {
			prefix = e.Date.Format("2006-01-02") + " - " // the publisher's own date, its offset kept
		}
		base[i] = prefix + Safe(e.Title, MaxFile-len(prefix)-len(ext)-9) + ext
		count[strings.ToLower(base[i])]++
	}
	for i, e := range eps {
		name := base[i]
		if name == "" {
			continue
		}
		eid := EpisodeID(e.GUID)
		if count[strings.ToLower(name)] > 1 || taken[strings.ToLower(name)] {
			ext := feed.Ext(e.Type, e.URL)
			name = strings.TrimSuffix(name, ext) + " [" + eid[:6] + "]" + ext
		}
		names[eid] = name
	}
	return names
}
