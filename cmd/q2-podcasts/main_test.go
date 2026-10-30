package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"q2-podcasts/internal/q2"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"q2-podcasts/internal/directory"
	"q2-podcasts/internal/q2spot"
)

// host is a podcast host and the directory: /feed.xml, its audio, and /search as iTunes answers.
func host(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<rss xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd"><channel><title>Night Shift</title>
<itunes:author>Nora</itunes:author>`)
		for i := 45; i >= 1; i-- { // more than one page
			fmt.Fprintf(w, `<item><title>Episode %d</title><guid>n%d</guid><pubDate>%s</pubDate><itunes:duration>%d</itunes:duration>
<enclosure url="%s/audio/%d.mp3" type="audio/mpeg" length="4096"/></item>`,
				i, i, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format(time.RFC1123Z), 600*i, srv.URL, i)
		}
		fmt.Fprint(w, `</channel></rss>`)
	})
	mux.HandleFunc("/audio/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte{0xff}, 4096))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"collectionName": "Night Shift", "artistName": "Nora", "feedUrl": srv.URL + "/feed.xml"},
			{"collectionName": "Apple Only", "artistName": "Nobody"}, // no feed: left out
		}})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func startApp(t *testing.T) (*app, string) {
	card := t.TempDir()
	socks, err := os.MkdirTemp("/tmp", "q2p") // a Unix socket's path is short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socks) })
	q2.CapsFile = filepath.Join(socks, "caps") // newer Q2 Pod's: the sleep feature offered
	os.WriteFile(q2.CapsFile, []byte("playlist progress pages library sleep\n"), 0o644)
	o := options{dir: filepath.Join(card, ".podcasts"), card: card, statusFile: filepath.Join(socks, "state"),
		browse: filepath.Join(socks, "b.sock"), control: filepath.Join(socks, "c.sock"), port: 18100,
		resume: filepath.Join(card, "ringnav-resume"), sleep: filepath.Join(socks, "sleep"), ready: func() bool { return true }}
	a, err := start(o)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.run(); close(done) }()
	t.Cleanup(func() { a.shutdown(); <-done })
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", o.browse); err == nil { // listening, not just bound
			c.Close()
			break
		}
		if time.Now().After(end) {
			t.Fatal("no browse socket")
		}
	}
	return a, card
}

type answer struct {
	status, message, cursor string
	rows                    [][6]string
}

var reqID = 0

// spot asks as addon.c does and checks the answer as its spot_reply does: the id echoed, four header
// fields and six a row, at most 50 rows in 64 KB, escapes valid; titles cut at 255 bytes.
func spot(t *testing.T, a *app, op string, args ...string) answer {
	t.Helper()
	reqID++
	c, err := net.Dial("unix", a.opts.browse)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	line := fmt.Sprintf("Q2SPOT/1\n%d\t%s", reqID, op)
	for _, s := range args {
		line += "\t" + q2spot.Escape(s)
	}
	c.Write([]byte(line + "\n"))
	c.(*net.UnixConn).CloseWrite()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	raw, _ := io.ReadAll(c)
	if len(raw) > q2spot.MaxReply {
		t.Fatalf("%s: %d bytes", op, len(raw))
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if lines[0] != "Q2SPOT/1" || len(lines) < 2 {
		t.Fatalf("%s: %q", op, raw)
	}
	unesc := func(s string) string {
		u, err := q2spot.Unescape(s)
		if err != nil {
			t.Fatalf("%s: bad escape %q", op, s)
		}
		return u
	}
	head := strings.Split(lines[1], "\t")
	if len(head) != 4 || head[0] != fmt.Sprint(reqID) {
		t.Fatalf("%s: header %q", op, lines[1])
	}
	r := answer{status: unesc(head[1]), message: unesc(head[2]), cursor: unesc(head[3])}
	if len(lines)-2 > q2spot.MaxRows {
		t.Fatalf("%s: %d rows", op, len(lines)-2)
	}
	for _, l := range lines[2:] {
		f := strings.Split(l, "\t")
		if len(f) != 6 {
			t.Fatalf("%s: row %q", op, l)
		}
		var row [6]string
		for i := range f {
			row[i] = unesc(f[i])
		}
		if len(row[2]) > 255 || len(row[3]) > 255 || !utf8.ValidString(row[2]) {
			t.Fatalf("%s: text %q", op, row)
		}
		r.rows = append(r.rows, row)
	}
	return r
}

func titles(r answer) []string {
	var out []string
	for _, row := range r.rows {
		out = append(out, row[2])
	}
	return out
}

func findRow(t *testing.T, r answer, title string) [6]string {
	t.Helper()
	for _, row := range r.rows {
		if row[2] == title {
			return row
		}
	}
	t.Fatalf("no row %q in %q", title, titles(r))
	return [6]string{}
}

// until asks again, as a "refresh" level is, until ok.
func until(t *testing.T, what string, ask func() answer, ok func(answer) bool) answer {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		r := ask()
		if ok(r) {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s: %q %q", what, r.message, titles(r))
		}
	}
}

func TestQ2Page(t *testing.T) {
	srv := host(t)
	directory.Base = srv.URL
	a, card := startApp(t)

	menu := spot(t, a, "menu")
	if strings.Join(titles(menu), "|") != "Shows|Sleep Timer|Search Podcasts|Add from Your Phone|Turn Off Podcasts" {
		t.Fatalf("empty menu %q", titles(menu))
	}
	if row := findRow(t, menu, "Search Podcasts"); row[0] != "search" {
		t.Errorf("search row %q", row)
	}
	if row := findRow(t, menu, "Add from Your Phone"); row[0] != "remote" {
		t.Errorf("remote row %q", row)
	}

	found := spot(t, a, "search", "night")
	if len(found.rows) != 1 || found.rows[0][0] != "list" || found.rows[0][1] != "preview" || found.rows[0][3] != "Nora" {
		t.Fatalf("search %q", found.rows)
	}
	key := found.rows[0][4]
	pre := until(t, "the preview", func() answer { return spot(t, a, "preview", key) },
		func(r answer) bool { return r.message != "refresh" })
	if sub := findRow(t, pre, "Subscribe"); sub[0] != "action" || sub[1] != "sub" || sub[3] != "45 episodes · Nora" {
		t.Fatalf("preview %q", pre.rows)
	}
	if r := spot(t, a, "sub", key); r.status != "ok" || r.message != "Subscribed to Night Shift" {
		t.Fatalf("sub %+v", r)
	}

	// Subscribed, the preview is the show's page: options, then the newest 40 and More….
	page := spot(t, a, "preview", key)
	if page.rows[0][2] != "Show Options" || page.rows[0][1] != "opts" || len(page.rows) != 41 || page.cursor != "40" {
		t.Fatalf("show page %d rows, cursor %q: %q", len(page.rows), page.cursor, titles(page)[:3])
	}
	showKey := page.rows[0][4]
	ep := page.rows[1]
	if ep[0] != "action" || ep[1] != "get" || ep[2] != "Episode 45" || !strings.HasPrefix(ep[3], "15 Feb · 7 hr 30 min · Download") &&
		!strings.HasPrefix(ep[3], "15 Feb 2024 · 7 hr 30 min · Download") {
		t.Fatalf("episode row %q", ep)
	}
	more := spot(t, a, "show", showKey, "40")
	if len(more.rows) != 5 || more.cursor != "" || more.rows[4][2] != "Episode 1" {
		t.Fatalf("more %q", titles(more))
	}

	if r := spot(t, a, "get", ep[4]); r.message != "Downloading" {
		t.Fatalf("get %+v", r)
	}
	page = until(t, "the download", func() answer { return spot(t, a, "show", showKey) },
		func(r answer) bool { return r.rows[1][0] == "local" })
	local := page.rows[1]
	want := filepath.Join(card, "Podcasts", "Night Shift", "2024-02-15 - Episode 45.mp3")
	if local[1] != want || local[4] != "album" || !strings.HasPrefix(local[5], "del:") || !strings.HasSuffix(local[3], "Downloaded") {
		t.Fatalf("local row %q", local)
	}
	// Tagged for Now Playing (the host's file has none), its audio as it came.
	got, err := os.ReadFile(want)
	if err != nil || !bytes.HasPrefix(got, []byte("ID3\x04")) || !bytes.HasSuffix(got, bytes.Repeat([]byte{0xff}, 4096)) ||
		!bytes.Contains(got, []byte("TIT2")) || !bytes.Contains(got, []byte("Episode 45")) || !bytes.Contains(got, []byte("Nora")) {
		t.Fatalf("on the card: %v, %d bytes", err, len(got))
	}

	// Played partway (Q2 Pod's resume ring holds its place): the row says what's left.
	ring := make([]byte, 4096)
	binary.LittleEndian.PutUint32(ring[0:], 7)
	binary.LittleEndian.PutUint32(ring[4:], 99)
	binary.LittleEndian.PutUint32(ring[8:], placeKey(want))
	binary.LittleEndian.PutUint32(ring[12:], 600)
	os.WriteFile(a.opts.resume, ring, 0o644)
	if r := spot(t, a, "show", showKey).rows[1]; r[1] != want || r[3] != "15 Feb 2024 · 7 hr 20 min left" {
		t.Fatalf("played row %q", r)
	}

	menu = spot(t, a, "menu")
	if d := findRow(t, menu, "Downloaded"); d[3] != "1 episode · 6 KB" {
		t.Errorf("menu downloaded %q", d)
	}
	if l := spot(t, a, "latest"); l.rows[0][2] != "Episode 45" || !strings.HasPrefix(l.rows[0][3], "Night Shift · ") {
		t.Errorf("latest %q", l.rows[0])
	}

	// Held Play/Pause deletes, the second hold confirming.
	if r := spot(t, a, local[5], "album"); r.message != "Hold again to delete it from the Q2" {
		t.Fatalf("first hold %+v", r)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal("deleted on the first hold")
	}
	if r := spot(t, a, local[5], "album"); r.message != "Deleted" {
		t.Fatalf("second hold %+v", r)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}

	opts := spot(t, a, "opts", showKey)
	if u := findRow(t, opts, "Unsubscribe"); u[1] != "unsub" {
		t.Fatalf("opts %q", titles(opts))
	}
	if f := findRow(t, opts, "Feed"); f[3] != strings.TrimPrefix(srv.URL, "http://") { // the host: no paid feed's key
		t.Errorf("feed row %q", f)
	}
	spot(t, a, "unsub", showKey)
	if findRow(t, spot(t, a, "opts", showKey), "Press Again to Unsubscribe")[1] != "unsub" {
		t.Fatal("not armed")
	}
	if r := spot(t, a, "unsub", showKey); r.message != "Unsubscribed" {
		t.Fatalf("unsub %+v", r)
	}
	if r := spot(t, a, "shows"); r.rows[0][2] != "No shows yet" {
		t.Fatalf("shows after %q", titles(r))
	}
	if r := spot(t, a, "nonsense"); r.status != "unsupported" {
		t.Errorf("unknown op: %+v", r)
	}
}

func TestSearchAddress(t *testing.T) {
	srv := host(t)
	a, _ := startApp(t)
	pre := until(t, "the address's preview", func() answer { return spot(t, a, "search", srv.URL+"/feed.xml") },
		func(r answer) bool { return r.message != "refresh" })
	if pre.rows[0][2] != "Subscribe" {
		t.Fatalf("%q", titles(pre))
	}
	bad := until(t, "the failure", func() answer { return spot(t, a, "search", srv.URL+"/missing.xml") },
		func(r answer) bool { return r.message != "refresh" })
	if bad.status != "error" || bad.message != "The feed isn't there any more (HTTP 404)" {
		t.Fatalf("%+v", bad)
	}
}

func TestPhonePage(t *testing.T) {
	srv := host(t)
	a, card := startApp(t)
	web := httptest.NewServer(newWeb(a))
	defer web.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	post := func(path, body string, x bool) *http.Response {
		req, _ := http.NewRequest("POST", web.URL+path, strings.NewReader(body))
		if x {
			req.Header.Set("X-Q2", "1")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp, _ := c.Get(web.URL + "/api/state"); resp.StatusCode != 401 {
		t.Fatalf("unsigned state: %d", resp.StatusCode)
	}
	if resp := post("/api/login", `{"PIN":"000000x"}`, true); resp.StatusCode != 403 {
		t.Fatalf("wrong PIN: %d", resp.StatusCode)
	}
	pin := a.settings().PIN
	if resp := post("/api/login", fmt.Sprintf(`{"PIN":"%s %s"}`, pin[:3], pin[3:]), true); resp.StatusCode != 200 {
		t.Fatalf("PIN: %d", resp.StatusCode)
	}
	if resp := post("/api/subscribe", `{"url":"`+srv.URL+`/feed.xml"}`, false); resp.StatusCode != 400 {
		t.Fatalf("without X-Q2: %d", resp.StatusCode)
	}
	resp := post("/api/subscribe", `{"url":"`+srv.URL+`/feed.xml"}`, true)
	var sh showJSON
	json.NewDecoder(resp.Body).Decode(&sh)
	if resp.StatusCode != 200 || sh.Title != "Night Shift" || sh.Episodes != 45 {
		t.Fatalf("subscribe %d %+v", resp.StatusCode, sh)
	}
	// A show's feed address (a paid feed's holds its key) never goes to the phone, over plain HTTP.
	resp, _ = c.Get(web.URL + "/api/state")
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("Night Shift")) || bytes.Contains(body, []byte("feed.xml")) {
		t.Fatalf("state %s", body)
	}
	resp, _ = c.Get(web.URL + "/api/opml")
	body, _ = io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte(`xmlUrl="`+srv.URL+`/feed.xml"`)) {
		t.Fatalf("export %s", body)
	}
	opml := `<opml version="2.0"><body><outline text="Other" xmlUrl="https://other.example/rss"/>` +
		`<outline text="Night" xmlUrl="` + srv.URL + `/feed.xml"/></body></opml>`
	resp = post("/api/opml", opml, true)
	var imp map[string]int
	json.NewDecoder(resp.Body).Decode(&imp)
	if imp["added"] != 1 || imp["found"] != 2 {
		t.Fatalf("import %v", imp)
	}
	if b, _ := os.ReadFile(filepath.Join(card, ".podcasts", "subscriptions.opml")); !bytes.Contains(b, []byte("other.example")) {
		t.Fatalf("saved %s", b)
	}
	// The QR code signs in by its key, once.
	resp, _ = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).
		Get(web.URL + "/?k=" + a.settings().Key)
	if resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Set-Cookie"), "HttpOnly") ||
		!strings.Contains(resp.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("key sign-in %d %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
}

func TestCardImportAndState(t *testing.T) {
	card := t.TempDir()
	os.WriteFile(filepath.Join(card, "podcasts.opml"),
		[]byte(`<opml><body><outline text="A" xmlUrl="https://a.example/rss"/></body></opml>`), 0o644)
	socks, _ := os.MkdirTemp("/tmp", "q2p")
	defer os.RemoveAll(socks)
	a, err := start(options{dir: filepath.Join(card, ".podcasts"), card: card, statusFile: filepath.Join(socks, "state"),
		browse: filepath.Join(socks, "b.sock"), control: filepath.Join(socks, "c.sock"), port: 18200})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.cancel(); a.lib.DL.Stop(); a.ln.Close() }()
	if shows := a.lib.Shows(); len(shows) != 1 || shows[0].Title != "A" {
		t.Fatalf("%+v", shows)
	}
	if _, err := os.Stat(filepath.Join(card, "podcasts.opml.imported")); err != nil {
		t.Error("not renamed")
	}
	raw, _ := os.ReadFile(filepath.Join(socks, "state"))
	for _, k := range []string{"hold=", "pin=", "version=q2-v"} {
		if !bytes.Contains(raw, []byte(k)) {
			t.Errorf("state lacks %s: %s", k, raw)
		}
	}
}

func TestFormats(t *testing.T) {
	for sec, want := range map[int]string{0: "", 42: "42 sec", 90: "2 min", 3599: "1 hr", 3600: "1 hr", 5400: "1 hr 30 min", 7170: "2 hr"} {
		if got := length(sec); got != want {
			t.Errorf("length(%d) = %q, want %q", sec, got, want)
		}
	}
	for b, want := range map[int64]string{0: "", 1000: "1 KB", 5 << 20: "5 MB", 3 << 29: "1.5 GB"} {
		if got := size(b); got != want {
			t.Errorf("size(%d) = %q", b, got)
		}
	}
	if join("", "a", "", "b") != "a · b" {
		t.Error("join")
	}
}

// Ten wrong PINs lock the page, each lockout twice as long as the last; the right PIN starts again.
func TestPINLockout(t *testing.T) {
	a, _ := startApp(t)
	w := &web{a: a}
	try := func(pin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"PIN":"`+pin+`"}`))
		r.Header.Set("X-Q2", "1")
		rec := httptest.NewRecorder()
		w.login(rec, r)
		return rec
	}
	wrong := "000000"
	if a.settings().PIN == wrong {
		wrong = "111111"
	}
	for _, want := range []string{"Wait 1 minute.", "Wait 2 minutes.", "Wait 4 minutes."} {
		for i := 0; i < 10; i++ {
			if c := try(wrong).Code; c != http.StatusForbidden {
				t.Fatal(want, i, c)
			}
		}
		if rec := try(a.settings().PIN); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), want) {
			t.Fatal(rec.Code, rec.Body.String())
		}
		w.mu.Lock()
		w.lockout = time.Time{} // the wait over
		w.mu.Unlock()
	}
	if try(a.settings().PIN).Code != http.StatusOK || w.locks != 0 {
		t.Fatal("the right PIN")
	}
}

// The app's key for a path is Q2 Pod's (navigation.c's resume ring): these from its own code.
func TestPlaceKey(t *testing.T) {
	for path, want := range map[string]uint32{
		"/mnt/mmc/Podcasts/Example Show Extras/2026-10-23 - Q&A - It's Bonus Time With Guests.mp3": 2032956665,
		"/mnt/mmc/Podcasts/Night Shift/2024-02-15 - Episode 45.mp3":                                1979994456,
	} {
		if got := placeKey(path); got != want {
			t.Errorf("%s: %d, want %d", path, got, want)
		}
	}
}

// The Sleep Timer: its choices, the request to Q2 Pod it writes (which Q2 Pod removes once it has
// paused), and the menu's word on it.
func TestSleepTimer(t *testing.T) {
	a, _ := startApp(t)
	if r := findRow(t, spot(t, a, "menu"), "Sleep Timer"); r[0] != "list" || r[1] != "sleep" || r[3] != "Off" {
		t.Fatalf("%q", r)
	}
	if p := spot(t, a, "sleep"); strings.Join(titles(p), "|") != "Off|15 Minutes|30 Minutes|45 Minutes|1 Hour|End of Episode" ||
		findRow(t, p, "Off")[3] != "Selected" {
		t.Fatalf("%q", p.rows)
	}
	if r := spot(t, a, "setsleep", "30"); r.message != "Pausing in 30 min" {
		t.Fatalf("%+v", r)
	}
	b, _ := os.ReadFile(a.opts.sleep)
	at, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(string(b), "until ")), 10, 64)
	if err != nil || at < time.Now().Add(29*time.Minute).Unix() || at > time.Now().Add(31*time.Minute).Unix() {
		t.Fatalf("%q", b)
	}
	if m := spot(t, a, "menu"); findRow(t, m, "Sleep Timer")[3] != "Pauses in 30 min" || m.message != "refresh" {
		t.Fatalf("%q", findRow(t, m, "Sleep Timer"))
	}
	if !strings.HasPrefix(findRow(t, spot(t, a, "sleep"), "30 Minutes")[3], "Selected · 30 min left") {
		t.Fatal("30 not selected")
	}
	spot(t, a, "setsleep", "end")
	if b, _ := os.ReadFile(a.opts.sleep); string(b) != "end\n" || findRow(t, spot(t, a, "menu"), "Sleep Timer")[3] != "Pauses at the end of this episode" {
		t.Fatalf("%q", b)
	}
	os.Remove(a.opts.sleep) // Q2 Pod paused, and removed it
	if findRow(t, spot(t, a, "menu"), "Sleep Timer")[3] != "Off" {
		t.Fatal("still on")
	}
	spot(t, a, "setsleep", "15")
	if r := spot(t, a, "setsleep", "off"); r.message != "Sleep Timer off" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(a.opts.sleep); !os.IsNotExist(err) {
		t.Fatal("not cancelled")
	}
	if r := spot(t, a, "setsleep", "never"); r.status != "bad" {
		t.Fatalf("%+v", r)
	}
}

// Firmware without a sleep timer (no "sleep" in its caps file, or no file: older Q2 Pod) gets no
// Sleep Timer row.
func TestSleepTimerNeedsFirmware(t *testing.T) {
	a, _ := startApp(t)
	for _, caps := range []string{"playlist progress pages library\n", ""} {
		os.Remove(q2.CapsFile)
		if caps != "" {
			os.WriteFile(q2.CapsFile, []byte(caps), 0o644)
		}
		for _, title := range titles(spot(t, a, "menu")) {
			if title == "Sleep Timer" {
				t.Fatalf("caps %q: Sleep Timer offered", caps)
			}
		}
	}
}
