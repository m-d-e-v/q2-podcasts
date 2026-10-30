package library

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"q2-podcasts/internal/feed"
	"q2-podcasts/internal/q2"
	"q2-podcasts/internal/tag"
)

// fake is a podcast host: a feed whose items the test sets, their audio, and a web page that names
// the feed.
type fake struct {
	*httptest.Server
	mu       sync.Mutex
	title    string
	items    []string // episode numbers, newest first
	etag     string
	token    string // set, the audio wants it: a paid feed's signed links, which expire
	gets     atomic.Int32
	notMod   atomic.Int32
	cutFirst atomic.Bool // the next audio request sends half and drops the connection
	cutFeed  atomic.Bool // the next feed request drops the connection inside its second item
	grows    atomic.Bool // after a cut, the audio is longer: an episode with ads put in as served
	mp3      atomic.Bool // the audio is MP3 frames, untagged (Patreon's)
	longer   atomic.Bool
	ranges   []string
	slow     chan struct{} // audio waits on this when set
	held     chan struct{} // set, the cover is asked for on it and never comes
}

func audio(n string) []byte { return bytes.Repeat([]byte("ep"+n+"-"), 1000) }

func mp3Audio(n string) []byte {
	return bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64, n[0], n[0], n[0], n[0]}, 1000)
}

// art is the show's cover, bigger than the Q2's.
func art() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 900, 900))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

func newFake(t *testing.T) *fake {
	f := &fake{title: "Test & Show: Live", items: []string{"2", "1"}, etag: `"v1"`}
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		f.gets.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("If-None-Match") == f.etag {
			f.notMod.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", f.etag)
		if f.cutFeed.CompareAndSwap(true, false) {
			var b bytes.Buffer
			fmt.Fprintf(&b, `<rss><channel><title>%s</title>`, strings.ReplaceAll(f.title, "&", "&amp;"))
			for _, n := range f.items {
				fmt.Fprintf(&b, `<item><title>Episode %s</title><guid>g%s</guid><enclosure url="%s/audio/%s.mp3" type="audio/mpeg"/></item>`, n, n, f.URL, n)
			}
			w.Header().Set("Content-Length", fmt.Sprint(b.Len()))
			w.Write(b.Bytes()[:bytes.LastIndex(b.Bytes(), []byte("<item>"))+40])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		fmt.Fprintf(w, `<rss xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>%s</title><itunes:author>Ann</itunes:author><itunes:image href="%s/art.jpg"/>`,
			strings.ReplaceAll(f.title, "&", "&amp;"), f.URL)
		for _, n := range f.items {
			fmt.Fprintf(w, `<item><title>Episode %s: Part/One?</title><guid>g%s</guid><pubDate>Mon, 0%s Oct 2024 09:00:00 +0000</pubDate>
<itunes:duration>30:00</itunes:duration><enclosure url="%s/audio/%s.mp3?tracker=1&amp;t=%s" type="audio/mpeg" length="%d"/></item>`,
				n, n, n, f.URL, n, f.token, len(audio(n)))
		}
		fmt.Fprint(w, `</channel></rss>`)
	})
	mux.HandleFunc("/art.jpg", func(w http.ResponseWriter, r *http.Request) {
		if f.held != nil {
			f.held <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Write(art())
	})
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><link rel="alternate" type="application/rss+xml" title="RSS" href="/feed.xml"></head></html>`)
	})
	mux.HandleFunc("/audio/", func(w http.ResponseWriter, r *http.Request) {
		n := strings.TrimSuffix(filepath.Base(r.URL.Path), ".mp3")
		if f.slow != nil {
			<-f.slow
		}
		f.mu.Lock()
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		expired := r.URL.Query().Get("t") != f.token
		f.mu.Unlock()
		if expired {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		if n == "404" {
			http.NotFound(w, r)
			return
		}
		data := audio(n)
		if f.mp3.Load() {
			data = mp3Audio(n)
		}
		if f.longer.Load() {
			data = append(data, data...)
		}
		if f.cutFirst.CompareAndSwap(true, false) {
			f.longer.Store(f.grows.Load())
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		http.ServeContent(w, r, n+".mp3", time.Time{}, bytes.NewReader(data))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func open(t *testing.T, dir string) *Library {
	l, err := Open(Options{Dir: filepath.Join(dir, ".podcasts"), Root: filepath.Join(dir, "Podcasts"),
		Client: &http.Client{Timeout: 5 * time.Second}, Fetch: &http.Client{}, Agent: "test", Ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.DL.Stop)
	return l
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestSubscribeAndReopen(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, err := l.Subscribe(context.Background(), f.URL+"/page") // a web page naming its feed
	if err != nil {
		t.Fatal(err)
	}
	if si.Title != "Test & Show: Live" || si.Folder != "Test & Show - Live" || si.Episodes != 2 || si.URL != f.URL+"/feed.xml" {
		t.Fatalf("%+v", si)
	}
	if again, err := l.Subscribe(context.Background(), f.URL+"/feed.xml"); err != nil || again.Key != si.Key {
		t.Fatalf("second subscribe: %+v %v", again, err)
	}
	eps, err := l.Episodes(si.Key)
	if err != nil || len(eps) != 2 || eps[0].Title != "Episode 2: Part/One?" ||
		filepath.Base(eps[0].Path) != "2024-10-02 - Episode 2 - Part-One.mp3" || eps[0].Duration != 1800 || eps[0].OnCard {
		t.Fatalf("%v %+v", err, eps)
	}
	if len(eps[0].Path) > 240 {
		t.Errorf("path %d bytes", len(eps[0].Path))
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".podcasts", "subscriptions.opml"))
	if !bytes.Contains(b, []byte(`q2key="`+si.Key+`"`)) {
		t.Errorf("opml: %s", b)
	}
	l.DL.Stop()
	l2 := open(t, dir)
	if shows := l2.Shows(); len(shows) != 1 || shows[0].Key != si.Key || shows[0].Episodes != 2 || shows[0].Folder != si.Folder {
		t.Fatalf("reopened: %+v", shows)
	}
}

func TestRefreshConditionalAndAuto(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, err := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Refresh(context.Background(), si.Key); err != nil || f.notMod.Load() != 1 {
		t.Fatalf("refresh: %v, %d not modified", err, f.notMod.Load())
	}
	l.SetAuto(si.Key, true)
	f.mu.Lock()
	f.items, f.etag = []string{"3", "2"}, `"v2"` // 1 dropped from the feed, 3 new
	f.mu.Unlock()
	if err := l.Refresh(context.Background(), si.Key); err != nil {
		t.Fatal(err)
	}
	eps, _ := l.Episodes(si.Key)
	if len(eps) != 3 || eps[0].Title != "Episode 3: Part/One?" || eps[2].Title != "Episode 1: Part/One?" {
		t.Fatalf("merged: %+v", eps)
	}
	if jobs := l.DL.Jobs(); len(jobs) != 1 || jobs[0].EID != eps[0].EID {
		t.Fatalf("auto: %+v", jobs)
	}
	l.DL.Start()
	waitFor(t, "the auto download", func() bool { e, _ := l.Episode(si.Key, eps[0].EID); return e.OnCard })
	got, _ := os.ReadFile(eps[0].Path)
	if !bytes.Equal(got, audio("3")) {
		t.Errorf("content %d bytes", len(got))
	}
	if st, _ := os.Stat(eps[0].Path); !st.ModTime().Equal(eps[0].Date) {
		t.Errorf("mtime %v", st.ModTime())
	}
}

// A feed cut off on the way keeps the episodes read whole, and isn't taken as the feed's version:
// the next refresh fetches it in full rather than hearing "not modified".
func TestFeedCutShort(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	f.cutFeed.Store(true)
	si, err := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	if eps, _ := l.Episodes(si.Key); err != nil || len(eps) != 1 {
		t.Fatalf("%v %+v", err, eps)
	}
	if err := l.Refresh(context.Background(), si.Key); err != nil || f.notMod.Load() != 0 {
		t.Fatalf("refresh: %v, %d not modified", err, f.notMod.Load())
	}
	if eps, _ := l.Episodes(si.Key); len(eps) != 2 {
		t.Fatalf("after the full fetch: %+v", eps)
	}
}

// An episode's address refused (a paid feed's signed link, expired) is fetched afresh from the
// feed, in full though the feed answers "not modified", and tried once more.
// A resume is only ever of the same file: one that changed size since is fetched whole again.
func TestResumeChanged(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	retryAfter = 10 * time.Millisecond
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	f.cutFirst.Store(true)
	f.grows.Store(true)
	l.DL.Queue(si.Key, eps[1].EID)
	l.DL.Start()
	waitFor(t, "the download", func() bool { e, _ := l.Episode(si.Key, eps[1].EID); return e.OnCard })
	got, _ := os.ReadFile(eps[1].Path)
	if want := append(audio("1"), audio("1")...); !bytes.Equal(got, want) {
		t.Fatalf("content %d bytes, want %d", len(got), len(want))
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".podcasts", "partial")); len(left) != 0 {
		t.Errorf("partial left: %v", left)
	}
}

// An MP3 the host sends untagged lands tagged for Now Playing: the episode's title, the show, its
// author and the show's cover, made small; its audio as it came. One downloaded untagged before
// (an older version's) is tagged by Retag.
func TestTagged(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	f.mp3.Store(true)
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Start()
	waitFor(t, "the download", func() bool { e, _ := l.Episode(si.Key, eps[0].EID); return e.OnCard })
	check := func(e EpisodeInfo, n string) {
		t.Helper()
		got, _ := os.ReadFile(e.Path)
		h, err := tag.Read(e.Path)
		if err != nil || h.Needs(true) || !h.HasCover() || !bytes.HasSuffix(got, mp3Audio(n)) ||
			!bytes.Contains(got, []byte(e.Title)) || !bytes.Contains(got, []byte("Test & Show: Live")) || !bytes.Contains(got, []byte("Ann")) {
			t.Fatalf("%s: %v", e.Title, err)
		}
		if i := bytes.Index(got, []byte{0xff, 0xd8}); i < 0 || len(got)-len(mp3Audio(n)) > 200<<10 {
			t.Fatal("no cover, or one not made small")
		}
	}
	check(eps[0], "2")
	if left, _ := os.ReadDir(filepath.Dir(eps[0].Path)); len(left) != 1 {
		t.Errorf("left beside it: %v", left)
	}
	os.WriteFile(eps[1].Path, mp3Audio("1"), 0o644)
	if n := l.Retag(context.Background()); n != 1 {
		t.Fatal("retagged", n)
	}
	check(eps[1], "1")
	if n := l.Retag(context.Background()); n != 0 {
		t.Fatal("retagged again", n)
	}
}

func TestExpiredLink(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, err := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	eps, _ := l.Episodes(si.Key)
	f.mu.Lock()
	f.token = "fresh" // the links read have expired; the ETag is the same
	f.mu.Unlock()
	l.mu.Lock()
	l.find(si.Key).c.Checked = time.Now().Add(-time.Hour)
	l.mu.Unlock()
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Start()
	waitFor(t, "the download", func() bool { e, _ := l.Episode(si.Key, eps[0].EID); return e.OnCard })
	if f.notMod.Load() != 0 {
		t.Error("asked conditionally")
	}
	// A feed fetched just now isn't fetched again: the refusal stands.
	f.mu.Lock()
	f.token = "newer"
	f.mu.Unlock()
	gets := f.gets.Load()
	l.DL.Queue(si.Key, eps[1].EID)
	waitFor(t, "the failure", func() bool { j := l.DL.Job(si.Key, eps[1].EID); return j != nil && j.State == Failed })
	if f.gets.Load() != gets {
		t.Error("fetched the feed again")
	}
}

// An episode an older version named by its whole link (no guid), or by a guid it shared, keeps that
// id: its file on the card stays its own.
func TestRenamed(t *testing.T) {
	prev := []feed.Episode{{GUID: "https://x/1.mp3?a", URL: "https://x/1.mp3?a", Title: "One"}, {GUID: "g", URL: "https://x/2.mp3", Title: "Two"}}
	eps := []feed.Episode{{GUID: "https://x/1.mp3", URL: "https://x/1.mp3?a", Title: "One"},
		{GUID: "g https://x/2.mp3", URL: "https://x/2.mp3", Title: "Two"}, {GUID: "g https://x/3.mp3", URL: "https://x/3.mp3", Title: "Three"}}
	renamed(prev, eps)
	if eps[0].GUID != "https://x/1.mp3?a" || eps[1].GUID != "g" || eps[2].GUID != "g https://x/3.mp3" {
		t.Fatalf("%+v", eps)
	}
}

func TestDownloadResumes(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	retryAfter = 10 * time.Millisecond
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	f.cutFirst.Store(true)
	if msg, err := l.DL.Queue(si.Key, eps[1].EID); err != nil || msg != "Downloading" {
		t.Fatal(msg, err)
	}
	l.DL.Start()
	waitFor(t, "the resumed download", func() bool { e, _ := l.Episode(si.Key, eps[1].EID); return e.OnCard })
	got, _ := os.ReadFile(eps[1].Path)
	if !bytes.Equal(got, audio("1")) {
		t.Fatalf("content %d bytes, want %d", len(got), len(audio("1")))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ranges) != 2 || f.ranges[0] != "" || f.ranges[1] != fmt.Sprintf("bytes=%d-", len(audio("1"))/2) {
		t.Errorf("ranges %q", f.ranges)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".podcasts", "partial")); len(left) != 0 {
		t.Errorf("partial left: %v", left)
	}
	if msg, _ := l.DL.Queue(si.Key, eps[1].EID); msg != "It's already on the Q2" {
		t.Errorf("queue again: %q", msg)
	}
}

func TestCancelAndUnsubscribe(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	f.slow = make(chan struct{})
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Queue(si.Key, eps[1].EID)
	l.DL.Start()
	waitFor(t, "the first to start", func() bool { return l.DL.Busy() })
	if !l.DL.Cancel(si.Key, eps[0].EID) {
		t.Fatal("cancel")
	}
	close(f.slow)
	waitFor(t, "the second", func() bool { e, _ := l.Episode(si.Key, eps[1].EID); return e.OnCard })
	if e, _ := l.Episode(si.Key, eps[0].EID); e.OnCard || e.Job != nil {
		t.Errorf("cancelled one: %+v", e)
	}
	if err := l.Unsubscribe(si.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(eps[1].Path); err != nil {
		t.Errorf("unsubscribing removed a download: %v", err)
	}
	if len(l.Shows()) != 0 {
		t.Error("still subscribed")
	}
}

func TestFailures(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	for _, c := range []struct{ url, want string }{
		{f.URL + "/nothing", "the feed isn't there any more (HTTP 404)"},
		{f.URL + "/audio/1.mp3", "that address isn't a podcast feed"},
		{"not a url at all", "that isn't a web address"},
	} {
		if _, err := l.Subscribe(context.Background(), c.url); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", c.url, err)
		}
	}
}

func TestDeleteAndDownloaded(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Queue(si.Key, eps[1].EID)
	l.DL.Start()
	waitFor(t, "both", func() bool { d, _ := l.Downloaded(); return len(d) == 2 })
	d, size := l.Downloaded()
	if d[0].EID != eps[0].EID || size != int64(len(audio("1"))+len(audio("2"))) {
		t.Errorf("%+v %d", d, size)
	}
	if shows := l.Shows(); shows[0].OnCard != 2 {
		t.Errorf("on card %d", shows[0].OnCard)
	}
	if err := l.Delete(si.Key, eps[0].EID); err != nil {
		t.Fatal(err)
	}
	if n, _, err := l.DeleteAll(si.Key); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Podcasts", si.Folder)); !os.IsNotExist(err) {
		t.Errorf("empty folder kept: %v", err)
	}
}

func TestImport(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	n := l.Import([]feed.Outline{{Title: "One", URL: f.URL + "/feed.xml"}, {Title: "Bad", URL: "::"}, {Title: "One", URL: f.URL + "/feed.xml"}})
	if n != 1 {
		t.Fatal(n)
	}
	shows := l.Shows()
	if shows[0].Fetched || shows[0].Title != "One" {
		t.Fatalf("%+v", shows)
	}
	if l.RefreshAll(context.Background(), time.Hour) != 1 {
		t.Fatal("refresh all")
	}
	if shows := l.Shows(); !shows[0].Fetched || shows[0].Title != "Test & Show: Live" || shows[0].Folder != "One" {
		t.Fatalf("after refresh: %+v", shows) // the folder stays as first named
	}
}

// Subscriptions it can't read are kept aside, not written over; two shows given one key both stay.
func TestOpenDamaged(t *testing.T) {
	dir := t.TempDir()
	opml := filepath.Join(dir, ".podcasts", "subscriptions.opml")
	os.MkdirAll(filepath.Dir(opml), 0o755)
	os.WriteFile(opml, []byte("<opml><body><outline xmlUrl=\"https://a.example/feed\"></opml"), 0o644)
	l := open(t, dir)
	if len(l.Shows()) != 0 {
		t.Fatal(l.Shows())
	}
	if b, err := os.ReadFile(opml + ".bad"); err != nil || !bytes.Contains(b, []byte("a.example")) {
		t.Fatal("not kept:", err)
	}
	l.DL.Stop()
	os.WriteFile(opml, feed.WriteOPML("x", []feed.Outline{{Title: "A", URL: "https://a.example/feed", Key: "k1"},
		{Title: "B", URL: "https://b.example/feed", Key: "k1"}, {Title: "A again", URL: "https://a.example/feed", Key: "k2"}}), 0o644)
	shows := open(t, dir).Shows()
	if len(shows) != 2 || shows[0].Key == shows[1].Key {
		t.Fatalf("%+v", shows)
	}
}

// A feed without a title doesn't rename its show; a show subscribed to without one has a name.
// A paid feed's key, in its address, stays out of the log.
func TestUntitledAndLog(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	f.title = ""
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	l := open(t, dir)
	si, err := l.Subscribe(context.Background(), f.URL+"/feed.xml?auth=SECRET")
	if err != nil || si.Title != "Untitled podcast" {
		t.Fatal(err, si.Title)
	}
	if strings.Contains(logged.String(), "SECRET") || !strings.Contains(logged.String(), "subscribed") {
		t.Errorf("log: %q", logged.String())
	}
	l.Import([]feed.Outline{{Title: "My Show", URL: f.URL + "/feed.xml"}})
	l.RefreshAll(context.Background(), time.Hour)
	for _, s := range l.Shows() {
		if s.URL == f.URL+"/feed.xml" && s.Title != "My Show" {
			t.Errorf("renamed %q", s.Title)
		}
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"A: B / C?":      "A - B - C",
		"..hidden.  ":    "hidden",
		"con":            "_con",
		"COM1":           "_COM1",
		"":               "Untitled",
		"tab\there\x01x": "tab here x",
		`<"quote">|pipe`: "'quote'-pipe",
	} {
		if got := Safe(in, 64); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	if got := Safe(strings.Repeat("é", 100), 63); len(got) > 63 || !strings.HasPrefix(got, "é") {
		t.Errorf("cut: %q", got)
	}
	day := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	names := fileNames([]feed.Episode{{GUID: "a", Title: "Same", Date: day, URL: "x.mp3"},
		{GUID: "b", Title: "Same", Date: day, URL: "y.m4a?z", Type: "audio/x-m4a"},
		{GUID: "c", Title: "Same", Date: day, URL: "z.mp3"}, {GUID: "d", Title: strings.Repeat("long ", 80), URL: "q"}}, nil)
	if names[EpisodeID("b")] != "2024-01-02 - Same.m4a" {
		t.Errorf("b: %q", names[EpisodeID("b")])
	}
	if a, c := names[EpisodeID("a")], names[EpisodeID("c")]; a == c || !strings.HasSuffix(a, "].mp3") || !strings.HasPrefix(a, "2024-01-02 - Same [") {
		t.Errorf("a %q c %q", a, c)
	}
	if d := names[EpisodeID("d")]; len(d) > MaxFile || !strings.HasSuffix(d, ".mp3") {
		t.Errorf("d: %d %q", len(d), d)
	}
	// A newcomer sharing a name already given takes the id; the one named first keeps its name.
	one := []feed.Episode{{GUID: "e", Title: "Bonus", Date: day, URL: "e.mp3"}}
	before := fileNames(one, nil)
	after := fileNames(append(one, feed.Episode{GUID: "f", Title: "Bonus", Date: day, URL: "f.mp3"}), before)
	if e, f := after[EpisodeID("e")], after[EpisodeID("f")]; e != "2024-01-02 - Bonus.mp3" || e != before[EpisodeID("e")] || !strings.HasSuffix(f, "].mp3") {
		t.Errorf("e %q f %q", e, f)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"example.com/feed":             "https://example.com/feed",
		" <http://example.com/f.xml> ": "http://example.com/f.xml",
		"feed://example.com/rss":       "http://example.com/rss",
		"feed:https://example.com/rss": "https://example.com/rss",
		"itpc://example.com/rss":       "http://example.com/rss",
		"podcast://example.com/rss":    "http://example.com/rss",
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "ftp://example.com/x", "two words"} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for s, want := range map[string]bool{"the daily": false, "example.com/feed": true, "www.x.org": true,
		"https://x": true, "Dr. Death": false, "radiolab": false} {
		if LooksLikeURL(s) != want {
			t.Errorf("LooksLikeURL(%q) = %v", s, !want)
		}
	}
}

func TestCardAwayCostsNoTry(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.mu.Lock()
	j := l.DL.jobs[0]
	j.State = Active
	l.DL.mu.Unlock()
	for i := 0; i < tries+2; i++ {
		l.DL.finish(j, q2.ErrNoCard)
		l.DL.mu.Lock()
		j.State = Active
		l.DL.mu.Unlock()
	}
	if ji := l.DL.Job(si.Key, eps[0].EID); ji == nil || ji.Err != "" || j.tries != 0 {
		t.Fatalf("after the card went away: %+v, %d tries", ji, j.tries)
	}
}

// An episode deleted while it is being tagged (its cover on its way) doesn't land on the card after.
func TestDeleteWhileTagging(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	f.mp3.Store(true)
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	f.held = make(chan struct{}, 1)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Start()
	select {
	case <-f.held: // whole, and being tagged
	case <-time.After(5 * time.Second):
		t.Fatal("no cover asked for")
	}
	if err := l.Delete(si.Key, eps[0].EID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cancel", func() bool { return !l.DL.Busy() })
	if left, _ := os.ReadDir(filepath.Dir(eps[0].Path)); len(left) != 0 {
		t.Errorf("on the card: %v", left)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".podcasts", "partial")); len(left) != 0 {
		t.Errorf("partial left: %v", left)
	}
}

// An episode asked for again while its cancelled download is on its way out is downloaded, not
// "already downloading" and then dropped with it.
func TestQueueAfterCancel(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.mu.Lock()
	old := l.DL.jobs[0]
	old.State = Active // under way
	l.DL.mu.Unlock()
	l.DL.Cancel(si.Key, eps[0].EID)
	if msg, _ := l.DL.Queue(si.Key, eps[0].EID); msg != "Downloading" {
		t.Fatalf("queued again: %q", msg)
	}
	l.DL.finish(old, context.Canceled) // the cancelled transfer's end
	if jobs := l.DL.Jobs(); len(jobs) != 1 || jobs[0].State != Waiting {
		t.Fatalf("after: %+v", jobs)
	}
	l.DL.Start()
	waitFor(t, "the download", func() bool { e, _ := l.Episode(si.Key, eps[0].EID); return e.OnCard })
}

// A file whose tag is too big to read (past 8 MB) is left as it is: a new tag would lose the
// publisher's.
func TestBigTagLeft(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	file := append([]byte{'I', 'D', '3', 4, 0, 0, 4, 0, 0, 0}, make([]byte, 8<<20)...) // 8 MB, syncsafe
	file = append(file, mp3Audio("1")...)
	os.MkdirAll(filepath.Dir(eps[1].Path), 0o755)
	os.WriteFile(eps[1].Path, file, 0o644)
	if n := l.Retag(context.Background()); n != 0 {
		t.Fatal("retagged", n)
	}
	if got, _ := os.ReadFile(eps[1].Path); !bytes.Equal(got, file) {
		t.Fatal("changed")
	}
}

// An MP3 that can't be tagged on the card (no room for the tagged copy; here a folder in its way)
// goes in untagged, for Retag, rather than failing with its whole .part kept.
func TestUntaggedWhenTagFails(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	f.mp3.Store(true)
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	eps, _ := l.Episodes(si.Key)
	os.MkdirAll(filepath.Join(tmpOf(eps[0].Path), "x"), 0o755)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Start()
	waitFor(t, "the download", func() bool { e, _ := l.Episode(si.Key, eps[0].EID); return e.OnCard })
	if got, _ := os.ReadFile(eps[0].Path); !bytes.Equal(got, mp3Audio("2")) {
		t.Fatalf("%d bytes", len(got))
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".podcasts", "partial")); len(left) != 0 {
		t.Errorf("partial left: %v", left)
	}
}

// A download of no known length stops, failed, once the card is down to its reserve; Retag leaves a
// file be while there's no room for its tagged copy.
func TestCardFull(t *testing.T) {
	var free atomic.Int64
	free.Store(reserve + 100<<10)
	was := freeBytes
	freeBytes = func(string) (int64, bool) { return free.Load(), true }
	t.Cleanup(func() { freeBytes = was })
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<rss><channel><title>Live</title><item><title>Live</title><guid>l</guid><enclosure url="%s/live.mp3" type="audio/mpeg"/></item></channel></rss>`, srv.URL)
	})
	mux.HandleFunc("/live.mp3", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 4; i++ { // no Content-Length
			w.Write(bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64}, 64<<10))
			w.(http.Flusher).Flush()
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	l := open(t, t.TempDir())
	si, err := l.Subscribe(context.Background(), srv.URL+"/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	eps, _ := l.Episodes(si.Key)
	l.DL.Queue(si.Key, eps[0].EID)
	l.DL.Start()
	waitFor(t, "the failure", func() bool { j := l.DL.Job(si.Key, eps[0].EID); return j != nil && j.State == Failed })
	if j := l.DL.Job(si.Key, eps[0].EID); j.Err != "the card is full" {
		t.Fatal(j.Err)
	}
	os.MkdirAll(filepath.Dir(eps[0].Path), 0o755)
	os.WriteFile(eps[0].Path, mp3Audio("1"), 0o644)
	free.Store(reserve)
	if n := l.Retag(context.Background()); n != 0 {
		t.Fatal("tagged without room", n)
	}
	free.Store(1 << 40)
	if n := l.Retag(context.Background()); n != 1 {
		t.Fatal("not tagged with room", n)
	}
}

// A feed checked "later" than now (the Q2's clock set back since) is stale, not left until the
// clock catches up.
func TestCheckedAhead(t *testing.T) {
	f, dir := newFake(t), t.TempDir()
	l := open(t, dir)
	si, _ := l.Subscribe(context.Background(), f.URL+"/feed.xml")
	if l.RefreshAll(context.Background(), time.Hour) != 0 {
		t.Fatal("refreshed a fresh feed")
	}
	l.mu.Lock()
	l.find(si.Key).c.Checked = time.Now().Add(24 * time.Hour).Round(0) // as read from the card
	l.mu.Unlock()
	if l.RefreshAll(context.Background(), time.Hour) != 1 {
		t.Fatal("not refreshed")
	}
}
