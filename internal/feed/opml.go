package feed

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"

	"q2-podcasts/internal/q2spot"
)

// Outline is one subscription in an OPML file, the format every podcast app imports and exports.
// Key, Folder and Auto are this app's own attributes (other apps ignore them): the show's id, kept
// when its feed moves, its folder on the card, and whether new episodes download by themselves.
type Outline struct {
	Title  string
	URL    string
	Key    string
	Folder string
	Auto   bool
}

type opmlDoc struct {
	XMLName xml.Name    `xml:"opml"`
	Version string      `xml:"version,attr"`
	Title   string      `xml:"head>title"`
	Body    []opmlEntry `xml:"body>outline"`
}

type opmlEntry struct {
	Text     string      `xml:"text,attr"`
	Title    string      `xml:"title,attr,omitempty"`
	Type     string      `xml:"type,attr,omitempty"`
	XMLURL   string      `xml:"xmlUrl,attr,omitempty"`
	Key      string      `xml:"q2key,attr,omitempty"`
	Folder   string      `xml:"q2folder,attr,omitempty"`
	Auto     string      `xml:"q2auto,attr,omitempty"`
	Children []opmlEntry `xml:"outline"`
}

// ParseOPML reads every outline with an xmlUrl, however deeply nested in folders, in order, each
// URL once.
func ParseOPML(r io.Reader) ([]Outline, error) {
	d := xml.NewDecoder(clean(r))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charsetReader
	var doc opmlDoc
	if err := d.Decode(&doc); err != nil {
		return nil, err
	}
	var out []Outline
	seen := map[string]bool{}
	var walk func([]opmlEntry)
	walk = func(es []opmlEntry) {
		for _, e := range es {
			if u := strings.TrimSpace(e.XMLURL); u != "" && !seen[u] {
				seen[u] = true
				t := strings.TrimSpace(e.Title)
				if t == "" {
					t = strings.TrimSpace(e.Text)
				}
				out = append(out, Outline{Title: q2spot.Cut(t, MaxText), URL: u, Key: e.Key, Folder: e.Folder,
					Auto: e.Auto == "1"})
			}
			walk(e.Children)
		}
	}
	walk(doc.Body)
	return out, nil
}

// WriteOPML is the subscriptions as OPML 2.0.
func WriteOPML(title string, subs []Outline) []byte {
	doc := opmlDoc{Version: "2.0", Title: title}
	for _, s := range subs {
		e := opmlEntry{Text: s.Title, Title: s.Title, Type: "rss", XMLURL: s.URL, Key: s.Key, Folder: s.Folder}
		if s.Auto {
			e.Auto = "1"
		}
		doc.Body = append(doc.Body, e)
	}
	var b bytes.Buffer
	b.WriteString(xml.Header)
	enc := xml.NewEncoder(&b)
	enc.Indent("", "  ")
	enc.Encode(doc)
	b.WriteByte('\n')
	return b.Bytes()
}
