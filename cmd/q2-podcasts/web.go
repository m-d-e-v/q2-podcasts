package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"q2-podcasts/internal/directory"
	"q2-podcasts/internal/feed"
	"q2-podcasts/internal/library"
)

//go:embed web/index.html
var indexHTML []byte

const cookieName = "q2pod"

// web is the phone's page: index.html and a JSON API behind the key the QR code carries (or the
// PIN on the Q2's screen), kept in a cookie. A change must carry X-Q2: 1, which a page elsewhere
// can't send here without asking first, and the cookie is SameSite=Strict.
type web struct {
	a *app

	mu      sync.Mutex
	fails   int
	locks   int // lockouts since the PIN was last right, each twice as long as the one before
	lockout time.Time
}

func serveWeb(a *app, l net.Listener) {
	srv := &http.Server{Handler: newWeb(a), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	srv.Serve(l)
}

func newWeb(a *app) http.Handler {
	w := &web{a: a}
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/api/login", w.login)
	mux.HandleFunc("/api/", w.api)
	return mux
}

func (w *web) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.a.settings().Key)) == 1
}

func (w *web) setCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: w.a.settings().Key, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
}

func (w *web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(w.a.settings().Key)) == 1 {
			w.a.touch(true)
			w.setCookie(rw)
		}
		http.Redirect(rw, r, "/", http.StatusFound)
		return
	}
	if w.authed(r) { // a page no one has signed in to keeps nothing awake
		w.a.touch(true)
	}
	h := rw.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src https: http: data:")
	rw.Write(indexHTML)
}

func reply(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func refuse(rw http.ResponseWriter, code int, msg string) {
	reply(rw, code, map[string]string{"error": msg})
}

// login takes the Q2's PIN. Ten wrong ones lock it for a minute, the next ten for two, then four…
func (w *web) login(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1024)).Decode(&body)
	w.mu.Lock()
	wait := time.Until(w.lockout)
	ok := wait <= 0 && subtle.ConstantTimeCompare([]byte(strings.ReplaceAll(strings.TrimSpace(body.PIN), " ", "")),
		[]byte(w.a.settings().PIN)) == 1
	if !ok && wait <= 0 {
		if w.fails++; w.fails >= 10 {
			w.lockout = time.Now().Add(time.Minute << min(w.locks, 10))
			w.fails, w.locks = 0, w.locks+1
		}
	}
	if ok {
		w.fails, w.locks = 0, 0
	}
	w.mu.Unlock()
	switch {
	case wait > 0:
		refuse(rw, http.StatusTooManyRequests, "Too many tries. Wait "+plural(int((wait+time.Minute-1)/time.Minute), "minute")+".")
	case !ok:
		refuse(rw, http.StatusForbidden, "That's not the PIN on the Q2's screen.")
	default:
		w.a.touch(true)
		w.setCookie(rw)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	}
}

// showJSON leaves the feed's address out: a paid feed's holds its key, and the page is plain HTTP.
type showJSON struct {
	Key        string    `json:"key"`
	Title      string    `json:"title"`
	Author     string    `json:"author,omitempty"`
	Image      string    `json:"image,omitempty"`
	Auto       bool      `json:"auto"`
	Episodes   int       `json:"episodes"`
	OnCard     int       `json:"onCard"`
	Latest     time.Time `json:"latest"`
	Checked    time.Time `json:"checked"`
	Refreshing bool      `json:"refreshing"`
	Err        string    `json:"error,omitempty"`
}

func showOut(s library.ShowInfo) showJSON {
	return showJSON{Key: s.Key, Title: s.Title, Author: s.Author, Image: s.Image, Auto: s.Auto,
		Episodes: s.Episodes, OnCard: s.OnCard, Latest: s.Latest, Checked: s.Checked, Refreshing: s.Refreshing, Err: s.Err}
}

type jobJSON struct {
	Key   string `json:"key"`
	EID   string `json:"eid"`
	Show  string `json:"show"`
	Title string `json:"title"`
	State string `json:"state"`
	Err   string `json:"error,omitempty"`
	Done  int64  `json:"done"`
	Size  int64  `json:"size"`
}

var states = map[library.JobState]string{library.Waiting: "waiting", library.Active: "active", library.Failed: "failed"}

func jobOut(j library.JobInfo) jobJSON {
	return jobJSON{Key: j.Key, EID: j.EID, Show: j.Show, Title: j.Title, State: states[j.State], Err: j.Err, Done: j.Done, Size: j.Size}
}

type episodeJSON struct {
	EID      string    `json:"eid"`
	Title    string    `json:"title"`
	Date     time.Time `json:"date"`
	Duration int       `json:"duration"`
	Length   int64     `json:"length"`
	OnCard   bool      `json:"onCard"`
	Size     int64     `json:"size"`
	Job      *jobJSON  `json:"job,omitempty"`
}

func (w *web) api(rw http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/api/")
	post := r.Method == http.MethodPost
	if !post && r.Method != http.MethodGet {
		refuse(rw, http.StatusMethodNotAllowed, "bad method")
		return
	}
	if post && r.Header.Get("X-Q2") != "1" {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	if !w.authed(r) {
		refuse(rw, http.StatusUnauthorized, "sign in")
		return
	}
	w.a.touch(true)
	lib := w.a.lib
	var body struct {
		URL string `json:"url"`
		Key string `json:"key"`
		EID string `json:"eid"`
		On  bool   `json:"on"`
	}
	var raw []byte
	if post {
		var err error
		if raw, err = io.ReadAll(http.MaxBytesReader(rw, r.Body, 4<<20)); err != nil {
			refuse(rw, http.StatusRequestEntityTooLarge, "That's too big")
			return
		}
		if route != "opml" && len(raw) > 0 && json.Unmarshal(raw, &body) != nil {
			refuse(rw, http.StatusBadRequest, "bad body")
			return
		}
	}
	fail := func(err error) {
		code := http.StatusBadGateway
		if errors.Is(err, library.ErrNoShow) || errors.Is(err, library.ErrNoEpisode) {
			code = http.StatusNotFound
		}
		refuse(rw, code, say(err))
	}
	switch {
	case route == "state" && !post:
		shows := []showJSON{}
		for _, s := range lib.Shows() {
			shows = append(shows, showOut(s))
		}
		jobs := []jobJSON{}
		for _, j := range lib.DL.Jobs() {
			jobs = append(jobs, jobOut(j))
		}
		eps, bytes := lib.Downloaded()
		reply(rw, http.StatusOK, map[string]any{"version": version, "shows": shows, "jobs": jobs,
			"downloaded": len(eps), "bytes": bytes, "refreshing": lib.Refreshing()})
	case route == "show" && !post:
		key := r.URL.Query().Get("key")
		si, ok := lib.Show(key)
		if !ok {
			fail(library.ErrNoShow)
			return
		}
		eps, _ := lib.Episodes(key)
		out := make([]episodeJSON, 0, len(eps))
		for _, e := range eps {
			ej := episodeJSON{EID: e.EID, Title: e.Title, Date: e.Date, Duration: e.Duration, Length: e.Length,
				OnCard: e.OnCard, Size: e.Size}
			if e.Job != nil {
				j := jobOut(*e.Job)
				ej.Job = &j
			}
			out = append(out, ej)
		}
		reply(rw, http.StatusOK, map[string]any{"show": showOut(si), "episodes": out})
	case route == "search" && !post:
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		found, err := directory.Search(ctx, w.a.client, strings.TrimSpace(r.URL.Query().Get("q")), 30)
		if err != nil {
			fail(err)
			return
		}
		out := []map[string]any{}
		for _, s := range found {
			_, sub := lib.Subscribed(s.FeedURL)
			out = append(out, map[string]any{"title": s.Title, "author": s.Author, "url": s.FeedURL,
				"genre": s.Genre, "episodes": s.Episodes, "subscribed": sub})
		}
		reply(rw, http.StatusOK, out)
	case route == "opml" && !post:
		rw.Header().Set("Content-Type", "text/x-opml; charset=utf-8")
		rw.Header().Set("Content-Disposition", `attachment; filename="q2-podcasts.opml"`)
		rw.Write(lib.OPML())
	case route == "opml":
		outlines, err := feed.ParseOPML(bytes.NewReader(raw))
		if err != nil || len(outlines) == 0 {
			refuse(rw, http.StatusBadRequest, "That isn't an OPML file with podcasts in it")
			return
		}
		n := lib.Import(outlines)
		go lib.RefreshAll(w.a.ctx, time.Hour)
		reply(rw, http.StatusOK, map[string]int{"added": n, "found": len(outlines)})
	case route == "subscribe" && post:
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		si, err := lib.Subscribe(ctx, body.URL)
		if err != nil {
			fail(err)
			return
		}
		reply(rw, http.StatusOK, showOut(si))
	case route == "unsubscribe" && post:
		if err := lib.Unsubscribe(body.Key); err != nil {
			fail(err)
			return
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "auto" && post:
		if err := lib.SetAuto(body.Key, body.On); err != nil {
			fail(err)
			return
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "refresh" && post:
		if body.Key == "" {
			go lib.RefreshAll(w.a.ctx, 0)
		} else {
			go lib.Refresh(w.a.ctx, body.Key)
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "download" && post:
		msg, err := lib.DL.Queue(body.Key, body.EID)
		if err != nil {
			fail(err)
			return
		}
		reply(rw, http.StatusOK, map[string]string{"message": msg})
	case route == "cancel" && post:
		lib.DL.Cancel(body.Key, body.EID)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "delete" && post:
		if err := lib.Delete(body.Key, body.EID); err != nil {
			fail(err)
			return
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(rw, r)
	}
}
