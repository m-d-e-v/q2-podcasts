package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"q2-podcasts/internal/directory"
	"q2-podcasts/internal/library"
	"q2-podcasts/internal/q2"
	"q2-podcasts/internal/q2spot"
)

// The Q2's Podcasts page (Q2 Pod's card app page, q2-pod's patch/addon.c). Each row's kind says
// what a press does: list opens the level named by its uri with its context as the argument, action
// sends its uri as an op with its context and toasts the reply's message, local plays from the card
// (here an episode, then the show's later ones: context album plays the folder in name order, and
// the names start with the date), search opens the Q2's keyboard, remote the QR code, info nothing.
// A row's position is the op a held Play/Pause sends. A reply whose message is "refresh" is asked
// again every 2 s while it is on screen, which is how progress moves.
//
// Ops: menu; shows; show <key> [cursor]; opts <key>; latest [cursor]; downloaded [cursor]; downloads;
// search <term>; preview <key> [cursor]. Actions: get, cancel <key:eid>; del:<key>:<eid> (held twice);
// sub, refresh1, auto, getlatest, wipe (twice), unsub (twice) <key>; refresh; retryall; cancelall;
// stop.

const (
	pageSize  = 40               // episodes a level shows before More…
	confirmIn = 5 * time.Second  // a second press within this confirms
	loadsKeep = 10 * time.Minute // a fetched preview is kept this long
)

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func list(op, arg, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: op, Context: arg, Title: title, Subtitle: sub}
}

func act(op, arg, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "action", URI: op, Context: arg, Title: title, Subtitle: sub}
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// say is an error as the Q2 shows it: a sentence.
func say(err error) string {
	s := err.Error()
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

// day is a date as a row's detail: "7 Oct", with the year when it isn't this one.
func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if t.Year() == time.Now().Year() {
		return t.Format("2 Jan")
	}
	return t.Format("2 Jan 2006")
}

func length(sec int) string {
	m := (sec + 30) / 60 // to the nearest minute
	switch {
	case sec <= 0:
		return ""
	case sec < 60:
		return fmt.Sprintf("%d sec", sec)
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d hr", m/60)
	}
	return fmt.Sprintf("%d hr %d min", m/60, m%60)
}

func size(b int64) string {
	switch {
	case b <= 0:
		return ""
	case b < 1<<20:
		return fmt.Sprintf("%d KB", (b+1023)>>10)
	case b < 1<<30:
		return fmt.Sprintf("%d MB", (b+1<<19)>>20)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "never"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	}
	return plural(int(d/(24*time.Hour)), "day") + " ago"
}

// join is the parts that aren't empty, separated as Q2 Pod's rows separate them.
func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func pct(done, total int64) string {
	if total <= 0 {
		return size(done)
	}
	return fmt.Sprintf("%d%% of %s", done*100/total, size(total))
}

// episodeRow is an episode: on the card it plays; otherwise a press downloads it, or cancels its
// download, or tries a failed one again.
// episodeRow is an episode's row; at: how far in Q2 Pod's place in it is, in seconds (0: none).
func episodeRow(e library.EpisodeInfo, withShow bool, at int) q2spot.Row {
	show := ""
	if withShow {
		show = e.Show
	}
	when := join(show, day(e.Date), length(e.Duration))
	id := e.Key + ":" + e.EID
	switch {
	case e.OnCard && at > 0: // part played: Q2 Pod resumes it there
		left := "Played " + length(at)
		if e.Duration > at {
			left = length(e.Duration-at) + " left"
		}
		return q2spot.Row{Kind: "local", URI: e.Path, Context: "album", Title: e.Title,
			Subtitle: join(show, day(e.Date), left), Position: "del:" + id}
	case e.OnCard:
		return q2spot.Row{Kind: "local", URI: e.Path, Context: "album", Title: e.Title,
			Subtitle: join(when, "Downloaded"), Position: "del:" + id}
	case e.Job == nil:
		dl := "Download"
		if e.Length > 0 {
			dl += " (" + size(e.Length) + ")"
		}
		return act("get", id, e.Title, join(when, dl))
	case e.Job.State == library.Active:
		return act("cancel", id, e.Title, join(show, "Downloading "+pct(e.Job.Done, e.Job.Size)))
	case e.Job.State == library.Failed:
		return act("get", id, e.Title, "Couldn't download: "+e.Job.Err+" · press to try again")
	case e.Job.Err != "":
		return act("cancel", id, e.Title, join(show, "Trying again soon", e.Job.Err))
	}
	return act("cancel", id, e.Title, join(show, "Waiting to download · press to cancel"))
}

// cursor is a paged op's offset: its last argument when it has more than skip (Q2 Pod before 1.4.2
// sent every earlier cursor too, so only the last counts).
func cursor(req q2spot.Request, skip int) int {
	if len(req.Args) <= skip {
		return 0
	}
	n, _ := strconv.Atoi(req.Args[len(req.Args)-1])
	return max(n, 0)
}

// page is eps[off:] up to pageSize as rows, and the cursor for More….
func (a *app) page(eps []library.EpisodeInfo, off int, withShow bool) ([]q2spot.Row, string) {
	var rows []q2spot.Row
	places := a.places()
	end := min(off+pageSize, len(eps))
	for i := off; i < end; i++ {
		at := 0
		if eps[i].OnCard {
			at = places[placeKey(eps[i].Path)]
		}
		rows = append(rows, episodeRow(eps[i], withShow, at))
	}
	if end < len(eps) {
		return rows, strconv.Itoa(end)
	}
	return rows, ""
}

// moving is whether any of eps is downloading or waiting to, so its level is asked again.
func moving(eps []library.EpisodeInfo) bool {
	for _, e := range eps {
		if e.Job != nil && e.Job.State != library.Failed {
			return true
		}
	}
	return false
}

func live(rows []q2spot.Row, cursor string, on bool) q2spot.Reply {
	r := q2spot.Reply{Rows: rows, Cursor: cursor}
	if on {
		r.Message = "refresh"
	}
	return r
}

// confirm is whether this press is the second within confirmIn of the first; the first arms it.
func (a *app) confirm(what string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at, ok := a.armed[what]; ok && time.Since(at) < confirmIn {
		delete(a.armed, what)
		return true
	}
	a.armed[what] = time.Now()
	return false
}

func (a *app) isArmed(what string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.armed[what]
	return ok && time.Since(at) < confirmIn
}

func (a *app) device(req q2spot.Request) q2spot.Reply {
	a.touch(false)
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }
	key := req.Arg(0)
	if strings.HasPrefix(req.Op, "del:") { // Play/Pause held on an episode on the card
		id := strings.TrimPrefix(req.Op, "del:")
		k, eid, _ := strings.Cut(id, ":")
		if !a.confirm("del:" + id) {
			return ok("Hold again to delete it from the Q2")
		}
		if err := a.lib.Delete(k, eid); err != nil {
			return q2spot.Fail(q2spot.Error, say(err))
		}
		return ok("Deleted")
	}
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"menu", "shows", "show", "opts", "latest", "downloaded", "downloads", "search",
			"preview", "get", "cancel", "del", "sub", "refresh", "refresh1", "auto", "getlatest", "wipe", "unsub",
			"retryall", "cancelall", "stop", "sleep", "setsleep"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return a.menu()
	case "sleep":
		return a.sleepPage()
	case "setsleep":
		return a.setSleep(key)
	case "shows":
		return a.shows()
	case "show":
		return a.show(key, cursor(req, 1))
	case "opts":
		return a.showOpts(key)
	case "latest":
		eps := a.lib.Latest(100)
		if len(eps) == 0 {
			return live([]q2spot.Row{info("No episodes yet", "Subscribe to a show first")}, "", a.lib.Refreshing() > 0)
		}
		rows, next := a.page(eps, cursor(req, 0), true)
		return live(rows, next, moving(eps) || a.lib.Refreshing() > 0)
	case "downloaded":
		eps, _ := a.lib.Downloaded()
		if len(eps) == 0 {
			return q2spot.Reply{Rows: []q2spot.Row{info("Nothing downloaded", "Press an episode to download it")}}
		}
		rows, next := a.page(eps, cursor(req, 0), true)
		return q2spot.Reply{Rows: rows, Cursor: next}
	case "downloads":
		return a.downloads()
	case "search":
		return a.search(strings.TrimSpace(key), cursor(req, 1))
	case "preview":
		return a.preview(key, cursor(req, 1))

	case "get":
		k, eid, _ := strings.Cut(key, ":")
		msg, err := a.lib.DL.Queue(k, eid)
		if err != nil {
			return q2spot.Fail(q2spot.Unavailable, say(err))
		}
		return ok(msg)
	case "cancel":
		k, eid, _ := strings.Cut(key, ":")
		a.lib.DL.Cancel(k, eid)
		return ok("Download cancelled")
	case "sub":
		return a.subscribe(key)
	case "refresh":
		go a.lib.RefreshAll(a.ctx, 0)
		return ok("Updating " + plural(len(a.lib.Shows()), "show"))
	case "refresh1":
		if _, found := a.lib.Show(key); !found {
			return q2spot.Fail(q2spot.Unavailable, say(library.ErrNoShow))
		}
		go a.lib.Refresh(a.ctx, key)
		return ok("Updating")
	case "auto":
		si, found := a.lib.Show(key)
		if !found {
			return q2spot.Fail(q2spot.Unavailable, say(library.ErrNoShow))
		}
		if err := a.lib.SetAuto(key, !si.Auto); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if si.Auto {
			return ok("New episodes won't download by themselves")
		}
		return ok("New episodes will download when Podcasts updates")
	case "getlatest":
		eps, err := a.lib.Episodes(key)
		if err != nil || len(eps) == 0 {
			return q2spot.Fail(q2spot.Unavailable, "No episodes to download")
		}
		msg, err := a.lib.DL.Queue(key, eps[0].EID)
		if err != nil {
			return q2spot.Fail(q2spot.Unavailable, say(err))
		}
		return ok(msg)
	case "wipe":
		if !a.confirm("wipe:" + key) {
			return ok("Press again to delete this show's downloads")
		}
		n, freed, err := a.lib.DeleteAll(key)
		if err != nil {
			return q2spot.Fail(q2spot.Error, say(err))
		}
		return ok(join("Deleted "+plural(n, "episode"), size(freed)))
	case "unsub":
		if !a.confirm("unsub:" + key) {
			return ok("Press again to unsubscribe")
		}
		if err := a.lib.Unsubscribe(key); err != nil {
			return q2spot.Fail(q2spot.Error, say(err))
		}
		return ok("Unsubscribed")
	case "retryall":
		return ok("Trying " + plural(a.lib.DL.RetryFailed(), "download") + " again")
	case "cancelall":
		return ok("Cancelled " + plural(a.lib.DL.CancelAll(), "download"))
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); a.shutdown() }() // the reply first
		return ok("Podcasts off")
	}
	return q2spot.Fail(q2spot.Unsupported, "Update Podcasts on the card")
}

func (a *app) menu() q2spot.Reply {
	shows := a.lib.Shows()
	jobs := a.lib.DL.Jobs()
	busy := a.lib.Refreshing()
	var rows []q2spot.Row
	if len(shows) > 0 {
		sub := ""
		if latest := a.lib.Latest(1); len(latest) > 0 {
			sub = join(latest[0].Show, day(latest[0].Date))
		}
		rows = append(rows, list("latest", "", "New Episodes", sub))
		rows = append(rows, list("shows", "", "Shows", plural(len(shows), "show")))
	} else {
		rows = append(rows, list("shows", "", "Shows", "None yet: search, or add from your phone"))
	}
	if eps, bytes := a.lib.Downloaded(); len(eps) > 0 {
		rows = append(rows, list("downloaded", "", "Downloaded", join(plural(len(eps), "episode"), size(bytes))))
	}
	if len(jobs) > 0 {
		rows = append(rows, list("downloads", "", "Downloads", jobsSummary(jobs)))
	}
	sleepSub, sleeping := a.sleepLine()
	if q2.HasCap("sleep") { // Q2 Pod keeps the timer only when it has it
		rows = append(rows, list("sleep", "", "Sleep Timer", sleepSub))
	}
	retagging := a.lib.Retagging()
	if retagging {
		rows = append(rows, info("Adding Titles and Covers…", "To episodes already on the Q2"))
	}
	rows = append(rows, q2spot.Row{Kind: "search", Title: "Search Podcasts", Subtitle: "By name, or type a feed's address"})
	phone := q2spot.Row{Kind: "remote", Title: "Add from Your Phone",
		Subtitle: "http://" + a.addr() + " · PIN " + spaced(a.settings().PIN)}
	if a.addr() == "" {
		phone.Subtitle = "Connect the Q2 to Wi-Fi first"
	}
	rows = append(rows, phone)
	if len(shows) > 0 {
		sub := "Updates every hour while Podcasts is on"
		if busy > 0 {
			sub = "Updating " + plural(busy, "show") + "…"
		} else if oldest := oldestCheck(shows); !oldest.IsZero() {
			sub = "Updated " + ago(oldest)
		}
		rows = append(rows, act("refresh", "", "Refresh All", sub))
	}
	idle := "It stays on until you turn it off"
	if m := a.settings().IdleMinutes; m > 0 {
		idle = fmt.Sprintf("It also turns off after %d minutes unused", m)
	}
	rows = append(rows, act("stop", "", "Turn Off Podcasts", idle))
	return live(rows, "", busy > 0 || len(jobs) > 0 || retagging || sleeping)
}

func oldestCheck(shows []library.ShowInfo) time.Time {
	var t time.Time
	for _, s := range shows {
		if s.Checked.IsZero() {
			continue
		}
		if t.IsZero() || s.Checked.Before(t) {
			t = s.Checked
		}
	}
	return t
}

func jobsSummary(jobs []library.JobInfo) string {
	var active *library.JobInfo
	waiting, failed := 0, 0
	for i := range jobs {
		switch jobs[i].State {
		case library.Active:
			active = &jobs[i]
		case library.Waiting:
			waiting++
		case library.Failed:
			failed++
		}
	}
	var parts []string
	if active != nil {
		parts = append(parts, "Downloading "+pct(active.Done, active.Size))
	}
	if waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d waiting", waiting))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	return join(parts...)
}

// spaced is a PIN as Q2 Pod's code page shows it: 548 001.
func spaced(pin string) string {
	if len(pin) == 6 {
		return pin[:3] + " " + pin[3:]
	}
	return pin
}

func (a *app) shows() q2spot.Reply {
	shows := a.lib.Shows()
	if len(shows) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("No shows yet", "Search, or add from your phone")}}
	}
	busy := false
	var rows []q2spot.Row
	for _, s := range shows {
		var sub string
		switch {
		case !s.Fetched && s.Refreshing:
			sub, busy = "Loading…", true
		case !s.Fetched && s.Err != "":
			sub = "Couldn't load: " + s.Err
		case !s.Fetched:
			sub = "Not loaded yet"
		default:
			on := ""
			if s.OnCard > 0 {
				on = fmt.Sprintf("%d downloaded", s.OnCard)
			}
			auto := ""
			if s.Auto {
				auto = "Auto"
			}
			sub = join("Latest "+day(s.Latest), on, auto)
			if s.Episodes == 0 {
				sub = join("No episodes", auto)
			}
			busy = busy || s.Refreshing
		}
		rows = append(rows, list("show", s.Key, s.Title, sub))
	}
	return live(rows, "", busy)
}

func (a *app) show(key string, off int) q2spot.Reply {
	si, found := a.lib.Show(key)
	if !found {
		return q2spot.Reply{Rows: []q2spot.Row{info("Not subscribed", "Press Return")}}
	}
	eps, _ := a.lib.Episodes(key)
	var rows []q2spot.Row
	if off == 0 {
		rows = append(rows, list("opts", key, "Show Options", "Update, auto-download, unsubscribe"))
		if !si.Fetched && si.Refreshing {
			return live(append(rows, info("Loading…", library.Host(si.URL))), "", true)
		}
		if si.Err != "" {
			rows = append(rows, info("Couldn't update", si.Err))
		}
		if len(eps) == 0 && si.Fetched {
			rows = append(rows, info("No episodes", "The feed has no audio episodes"))
		}
	}
	more, next := a.page(eps, off, false)
	return live(append(rows, more...), next, si.Refreshing || moving(eps))
}

func (a *app) showOpts(key string) q2spot.Reply {
	si, found := a.lib.Show(key)
	if !found {
		return q2spot.Reply{Rows: []q2spot.Row{info("Unsubscribed", "Its downloads stay in Library → Podcasts")}}
	}
	updated := "Updated " + ago(si.Checked)
	if si.Refreshing {
		updated = "Updating…"
	}
	auto := act("auto", key, "Download New Episodes: Off", "Turn on to download each new one when Podcasts updates")
	if si.Auto {
		auto = act("auto", key, "Download New Episodes: On", "Each new one downloads when Podcasts updates")
	}
	rows := []q2spot.Row{act("refresh1", key, "Update Now", updated), auto}
	eps, _ := a.lib.Episodes(key)
	if len(eps) > 0 {
		rows = append(rows, act("getlatest", key, "Download Latest Episode", eps[0].Title))
	}
	on, bytes := 0, int64(0)
	for _, e := range eps {
		if e.OnCard {
			on++
			bytes += e.Size
		}
	}
	if on > 0 {
		title := "Delete Downloads"
		if a.isArmed("wipe:" + key) {
			title = "Press Again to Delete"
		}
		rows = append(rows, act("wipe", key, title, join(plural(on, "episode"), size(bytes))))
	}
	title := "Unsubscribe"
	if a.isArmed("unsub:" + key) {
		title = "Press Again to Unsubscribe"
	}
	rows = append(rows, act("unsub", key, title, "Downloaded episodes stay on the Q2"))
	rows = append(rows, info("Feed", library.Host(si.URL))) // the rest may hold a paid feed's key
	if si.Author != "" {
		rows = append(rows, info("By", si.Author))
	}
	return live(rows, "", si.Refreshing)
}

func (a *app) downloads() q2spot.Reply {
	jobs := a.lib.DL.Jobs()
	if len(jobs) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing downloading", "Press an episode to download it")}}
	}
	var rows []q2spot.Row
	failed := 0
	for _, j := range jobs {
		id := j.Key + ":" + j.EID
		switch j.State {
		case library.Active:
			rows = append(rows, act("cancel", id, j.Title, join(j.Show, "Downloading "+pct(j.Done, j.Size))))
		case library.Failed:
			failed++
			rows = append(rows, act("get", id, j.Title, "Couldn't download: "+j.Err))
		default:
			why := "Waiting"
			if j.Err != "" {
				why = "Trying again soon: " + j.Err
			}
			rows = append(rows, act("cancel", id, j.Title, join(j.Show, why)))
		}
	}
	if failed > 0 {
		rows = append(rows, act("retryall", "", "Try Failed Again", plural(failed, "download")))
	}
	rows = append(rows, act("cancelall", "", "Cancel All", plural(len(jobs), "download")))
	if len(rows) > q2spot.MaxRows {
		rows = append(rows[:q2spot.MaxRows-2], rows[len(rows)-2:]...)
	}
	return live(rows, "", true)
}

// search finds shows by name in the directory, or, for an address (a feed, a podcast's web page or
// an Apple Podcasts link), previews that feed.
func (a *app) search(term string, off int) q2spot.Reply {
	if term == "" {
		return q2spot.Fail(q2spot.Bad, "Type a name or an address")
	}
	if library.LooksLikeURL(term) || directory.AppleID(term) != "" {
		key := library.KeyFor(term)
		a.remember(key, term)
		return a.preview(key, off)
	}
	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Second)
	defer cancel()
	found, err := directory.Search(ctx, a.client, term, 30)
	if err != nil {
		return q2spot.Fail(q2spot.Offline, say(err))
	}
	if len(found) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("No podcasts found", "Try other words, or type a feed's address")}}
	}
	var rows []q2spot.Row
	for _, s := range found {
		key := library.KeyFor(s.FeedURL)
		a.remember(key, s.FeedURL)
		sub := s.Author
		if _, ok := a.lib.Subscribed(s.FeedURL); ok {
			sub = join("Subscribed", s.Author)
		}
		rows = append(rows, list("preview", key, s.Title, sub))
	}
	return q2spot.Reply{Rows: rows}
}

func (a *app) remember(key, what string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.found) > 500 {
		a.found = map[string]string{}
	}
	a.found[key] = what
}

// load is a preview's feed being fetched, or fetched.
type load struct {
	done bool
	r    *library.Fetched
	err  error
	at   time.Time
}

// fetchFeed starts fetching what (an address or an Apple Podcasts link) once; done when it has
// ended. A failure is reported once and forgotten, so Retry fetches again.
func (a *app) fetchFeed(what string) (*library.Fetched, error, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, l := range a.loads {
		if l.done && time.Since(l.at) > loadsKeep {
			delete(a.loads, k)
		}
	}
	l := a.loads[what]
	if l == nil {
		l = &load{at: time.Now()}
		a.loads[what] = l
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 90*time.Second)
			defer cancel()
			u, err := a.lib.Resolve(ctx, what)
			var r *library.Fetched
			if err == nil {
				r, err = a.lib.Preview(ctx, u)
			}
			a.mu.Lock()
			l.r, l.err, l.done, l.at = r, err, true, time.Now()
			a.mu.Unlock()
		}()
	}
	if l.done && l.err != nil {
		delete(a.loads, what)
	}
	return l.r, l.err, l.done
}

func (a *app) subscribedTo(what string) (library.ShowInfo, bool) {
	if u, err := library.Normalize(what); err == nil {
		return a.lib.Subscribed(u)
	}
	return library.ShowInfo{}, false
}

// preview is a search result: its episodes and Subscribe, or, once subscribed, the show's page.
func (a *app) preview(key string, off int) q2spot.Reply {
	a.mu.Lock()
	what := a.found[key]
	a.mu.Unlock()
	if what == "" {
		return q2spot.Reply{Rows: []q2spot.Row{info("Search again", "This result has expired")}}
	}
	if si, ok := a.subscribedTo(what); ok {
		return a.show(si.Key, off)
	}
	r, err, done := a.fetchFeed(what)
	if !done {
		return live([]q2spot.Row{info("Loading…", "Fetching the feed")}, "", true)
	}
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	if si, ok := a.lib.Subscribed(r.URL); ok {
		return a.show(si.Key, off)
	}
	f := r.Feed
	rows := []q2spot.Row{act("sub", key, "Subscribe", join(plural(len(f.Episodes), "episode"), f.Author))}
	for i, e := range f.Episodes {
		if i == pageSize {
			break
		}
		rows = append(rows, info(e.Title, join(day(e.Date), length(e.Duration))))
	}
	return q2spot.Reply{Rows: rows}
}

func (a *app) subscribe(key string) q2spot.Reply {
	a.mu.Lock()
	what := a.found[key]
	a.mu.Unlock()
	if what == "" {
		return q2spot.Fail(q2spot.Unavailable, "Search again: this result has expired")
	}
	r, err, done := a.fetchFeed(what)
	switch {
	case !done:
		return q2spot.Reply{Message: "Still loading: try again in a moment"}
	case err != nil:
		return q2spot.Fail(q2spot.Error, say(err))
	}
	si := a.lib.Adopt(r)
	return q2spot.Reply{Message: "Subscribed to " + si.Title}
}
