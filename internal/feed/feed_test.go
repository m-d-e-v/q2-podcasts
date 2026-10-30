package feed

import (
	"bytes"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const rss = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:atom="http://www.w3.org/2005/Atom">
<channel>
  <atom:link href="https://example.com/feed.xml" rel="self" type="application/rss+xml"/>
  <title>The  Example
   Show</title>
  <link>https://example.com/</link>
  <itunes:title>Ignored when there is a plain title</itunes:title>
  <itunes:author>Ann Example</itunes:author>
  <itunes:image href="https://example.com/art.jpg"/>
  <itunes:new-feed-url>https://new.example.com/feed.xml</itunes:new-feed-url>
  <description><![CDATA[<p>Long show notes &nbsp; that are never kept</p>]]></description>
  <item>
    <title>Episode 1: Hello &amp; welcome</title>
    <guid isPermaLink="false">ep-1</guid>
    <pubDate>Mon, 07 Oct 2024 09:00:00 +0000</pubDate>
    <itunes:duration>1:02:03</itunes:duration>
    <itunes:image href="https://example.com/ep1.jpg"/>
    <enclosure url="https://cdn.example.com/ep1.mp3?src=rss" type="audio/mpeg" length="12345678"/>
    <description>notes &nbsp; &mdash; with HTML entities</description>
  </item>
  <item>
    <title><![CDATA[Episode 3 <b>bold</b>]]></title>
    <guid>ep-3</guid>
    <pubDate>Wednesday, 9 Oct 2024 08:30 EDT</pubDate>
    <itunes:duration>2700</itunes:duration>
    <enclosure url="https://cdn.example.com/ep3.m4a" type="audio/x-m4a" length=""/>
  </item>
  <item>
    <title>Episode 2 (video)</title>
    <guid>ep-2v</guid>
    <pubDate>Tue, 08 Oct 2024 09:00:00 GMT</pubDate>
    <enclosure url="https://cdn.example.com/ep2.mp4" type="video/mp4" length="1"/>
  </item>
  <item>
    <title>A text post</title>
    <guid>post</guid>
  </item>
  <item>
    <title>Episode 1: Hello &amp; welcome</title>
    <guid isPermaLink="false">ep-1</guid>
    <enclosure url="https://cdn.example.com/ep1-dup.mp3" type="audio/mpeg"/>
  </item>
  <item>
    <itunes:title>Only an itunes title</itunes:title>
    <pubDate>2024-10-10T12:00:00Z</pubDate>
    <enclosure url="https://cdn.example.com/ep4.mp3" length="99"/>
  </item>
</channel>
</rss>`

func TestRSS(t *testing.T) {
	f, err := Parse(strings.NewReader(rss))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "The Example Show" || f.Author != "Ann Example" || f.Image != "https://example.com/art.jpg" ||
		f.NewURL != "https://new.example.com/feed.xml" {
		t.Errorf("channel: %+v", f)
	}
	var got []string
	for _, e := range f.Episodes {
		got = append(got, e.GUID)
	}
	// Newest first; no video, no text post; a guid given twice told apart by the links.
	if strings.Join(got, ",") != "https://cdn.example.com/ep4.mp3,ep-3,ep-1 https://cdn.example.com/ep1.mp3,ep-1 https://cdn.example.com/ep1-dup.mp3" {
		t.Fatalf("episodes %v", got)
	}
	e4, e3, e1 := f.Episodes[0], f.Episodes[1], f.Episodes[2]
	if e1.Title != "Episode 1: Hello & welcome" || e1.Duration != 3723 || e1.Length != 12345678 || e1.Image != "https://example.com/ep1.jpg" ||
		e1.URL != "https://cdn.example.com/ep1.mp3?src=rss" || !e1.Date.Equal(time.Date(2024, 10, 7, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("episode 1: %+v", e1)
	}
	if e3.Title != "Episode 3 <b>bold</b>" || e3.Duration != 2700 || e3.Length != 0 ||
		!e3.Date.Equal(time.Date(2024, 10, 9, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("episode 3: %+v (%v)", e3, e3.Date.UTC())
	}
	if e4.Title != "Only an itunes title" || e4.Type != "" {
		t.Errorf("episode 4: %+v", e4)
	}
}

// Episodes are told apart however their feed names them; one listed twice is kept once.
func TestIdentify(t *testing.T) {
	item := func(guid, url string) string {
		return `<item><title>T</title><guid>` + guid + `</guid><enclosure url="` + url + `" type="audio/mpeg"/></item>`
	}
	feed := `<rss><channel><title>S</title>` +
		item("https://example.com/", "https://x/1.mp3?sig=a") + item("https://example.com/", "https://x/2.mp3?sig=b") +
		item("", "https://x/get.php?id=7") + item("", "https://x/get.php?id=8") + item("", "https://y/3.mp3?sig=c") +
		item("same", "https://x/4.mp3") + item("same", "https://x/4.mp3") + `</channel></rss>`
	f, err := Parse(strings.NewReader(feed))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range f.Episodes {
		got = append(got, e.GUID)
	}
	want := "https://example.com/ https://x/1.mp3,https://example.com/ https://x/2.mp3," +
		"https://x/get.php https://x/get.php?id=7,https://x/get.php https://x/get.php?id=8,https://y/3.mp3,same"
	if strings.Join(got, ",") != want {
		t.Fatalf("%q", got)
	}
}

func TestAtom(t *testing.T) {
	const atom = `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Atom Cast</title>
  <link rel="self" href="https://a.example/feed"/>
  <link href="https://a.example/"/>
  <author><name>Bea</name></author>
  <entry>
    <title>First</title><id>urn:1</id><updated>2024-01-02T03:04:05Z</updated>
    <link rel="alternate" href="https://a.example/1"/>
    <link rel="enclosure" href="https://a.example/1.ogg" type="audio/ogg" length="42"/>
  </entry>
  <entry><title>No audio</title><id>urn:2</id></entry>
</feed>`
	f, err := Parse(strings.NewReader(atom))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Atom Cast" || f.Author != "Bea" || len(f.Episodes) != 1 {
		t.Fatalf("%+v", f)
	}
	if e := f.Episodes[0]; e.GUID != "urn:1" || e.URL != "https://a.example/1.ogg" || e.Length != 42 || e.Date.Year() != 2024 {
		t.Errorf("%+v", e)
	}
}

func TestLatin1AndControls(t *testing.T) {
	doc := "\xef\xbb\xbf\n  <?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss><channel><title>Caf\xe9 \x93Talk\x94\x08</title>" +
		"<item><title>Ep</title><enclosure url=\"http://x/1.mp3\" type=\"audio/mpeg\"/></item></channel></rss>"
	f, err := Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Café “Talk”" {
		t.Errorf("title %q", f.Title)
	}
	// Said to be UTF-8 (or nothing said), a byte that isn't is read as Windows-1252; real UTF-8 split
	// across reads stays as it is.
	for _, prolog := range []string{`<?xml version="1.0" encoding="UTF-8"?>`, ""} {
		doc := prolog + "<rss><channel><title>Caf\xe9 \x93Talk\x94 \u00fcber \u2014 \U0001F399</title>" +
			"<item><title>Ep</title><enclosure url=\"http://x/1.mp3\" type=\"audio/mpeg\"/></item></channel></rss>"
		f, err := Parse(iotest.OneByteReader(strings.NewReader(doc)))
		if err != nil || f.Title != "Café “Talk” über — 🎙" || len(f.Episodes) != 1 {
			t.Errorf("%q: %v %q", prolog, err, f.Title)
		}
	}
}

func TestNotAFeed(t *testing.T) {
	for _, doc := range []string{"<html><body>Not here</body></html>", "", "{\"json\": true}"} {
		if _, err := Parse(strings.NewReader(doc)); err != ErrNotFeed {
			t.Errorf("%q: %v", doc, err)
		}
	}
}

func TestTruncated(t *testing.T) {
	cut := rss[:strings.Index(rss, "<title>Episode 2 (video)")+30]
	f, err := Parse(strings.NewReader(cut))
	if err != nil || len(f.Episodes) != 2 {
		t.Fatalf("%v %+v", err, f)
	}
	// An item cut off after its enclosure is left out: its guid and title may not have come yet.
	cut = rss[:strings.Index(rss, `length="99"/>`)+16]
	if f, err := Parse(strings.NewReader(cut)); err != nil || len(f.Episodes) != 3 {
		t.Fatalf("%v %+v", err, f)
	}
}

func TestDates(t *testing.T) {
	want := time.Date(2024, 3, 5, 14, 7, 0, 0, time.UTC)
	for _, s := range []string{
		"Tue, 05 Mar 2024 14:07:00 +0000", "Tue, 5 Mar 2024 14:07:00 GMT", "5 Mar 2024 14:07:00 +0000",
		"Tuesday, 05 March 2024 14:07:00 +0000", "Tue, 05 Mar 2024 09:07:00 EST", "Tue, 05 Mar 2024 14:07 +0000",
		"2024-03-05T14:07:00Z", "2024-03-05T15:07:00+01:00", "2024-03-05T14:07:00.000Z", "Tue, 05 Mar 2024 14:07:00 +00:00",
		"2024-03-05 14:07:00",
	} {
		if got := Date(s); !got.Equal(want) {
			t.Errorf("%q: %v", s, got)
		}
	}
	if got := Date("Thu, 5 Sept 2024"); got.Month() != time.September || got.Day() != 5 {
		t.Errorf("Sept: %v", got)
	}
	if !Date("whenever").IsZero() || !Date("").IsZero() {
		t.Error("nonsense parsed")
	}
}

func TestDuration(t *testing.T) {
	for s, want := range map[string]int{"3723": 3723, "3723.6": 3723, "62:03": 3723, "1:02:03": 3723, "01:02:03.000": 3723,
		"": 0, "1h": 0, "1:2:3:4": 0, "-5": 0} {
		if got := Duration(s); got != want {
			t.Errorf("%q: %d", s, got)
		}
	}
}

func TestAudioAndExt(t *testing.T) {
	for _, c := range []struct {
		typ, url string
		audio    bool
		ext      string
	}{
		{"audio/mpeg", "https://x/a.mp3?x=1", true, ".mp3"},
		{"audio/mpeg", "https://x/download?id=5", true, ".mp3"},
		{"audio/x-m4a", "https://x/a", true, ".m4a"},
		{"audio/mp4", "https://x/a.mp4", true, ".m4a"},
		{"", "https://x/a.mp4", true, ".m4a"},
		{"video/mp4", "https://x/a.mp4", false, ".m4a"},
		{"", "https://x/a.ogg#t", true, ".ogg"},
		{"application/octet-stream", "https://x/a.opus", true, ".opus"},
		{"", "https://x/page.html", false, ".mp3"},
		{"audio/aac; charset=x", "https://x/a", true, ".aac"},
	} {
		if Audio(c.typ, c.url) != c.audio || Ext(c.typ, c.url) != c.ext {
			t.Errorf("%+v: %v %s", c, Audio(c.typ, c.url), Ext(c.typ, c.url))
		}
	}
}

func TestOPML(t *testing.T) {
	const doc = `<?xml version="1.0"?><opml version="1.1"><head><title>Mine</title></head><body>
<outline text="Folder"><outline text="One" type="rss" xmlUrl="https://1.example/f"/>
<outline title="Two" text="ignored" xmlUrl=" https://2.example/f "/></outline>
<outline text="One again" xmlUrl="https://1.example/f"/><outline text="Not a feed"/></body></opml>`
	got, err := ParseOPML(strings.NewReader(doc))
	if err != nil || len(got) != 2 || got[0] != (Outline{Title: "One", URL: "https://1.example/f"}) ||
		got[1] != (Outline{Title: "Two", URL: "https://2.example/f"}) {
		t.Fatalf("%v %+v", err, got)
	}
	subs := []Outline{{Title: "A & B", URL: "https://a.example/f?x=1&y=2", Key: "0123456789", Folder: "A & B", Auto: true}, {Title: "C", URL: "https://c.example/"}}
	out := WriteOPML("Q2 Podcasts", subs)
	if !bytes.Contains(out, []byte(`xmlUrl="https://a.example/f?x=1&amp;y=2"`)) {
		t.Errorf("%s", out)
	}
	back, err := ParseOPML(bytes.NewReader(out))
	if err != nil || len(back) != 2 || back[0] != subs[0] || back[1] != subs[1] {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}
