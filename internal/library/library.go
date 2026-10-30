// Package library keeps the podcasts: the subscriptions (an OPML file on the card), each feed as
// last fetched, and the episodes downloaded into the card's Podcasts folder, where Q2 Pod resumes
// each where it stopped, keeps a folder's playback speed and leaves them out of play counts.
//
//	<dir>/subscriptions.opml   the shows, in OPML any podcast app imports
//	<dir>/feeds/<key>.json     each show's feed as last fetched, with its ETag
//	<dir>/partial/<key>-<eid>.part  a download under way, resumed after a break
//	<dir>/queue.json           the downloads waiting
//	<root>/<Show>/2024-10-07 - Title.mp3  the episodes
package library

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"q2-podcasts/internal/directory"
	"q2-podcasts/internal/feed"
	"q2-podcasts/internal/q2"
)

const (
	MaxEpisodes = 500      // kept a show, newest first; older ones the feed still lists are dropped
	MaxFeed     = 32 << 20 // bytes of a feed read; a longer one keeps the episodes read by then
	autoMax     = 3        // new episodes one refresh downloads by itself
)

type Options struct {
	Dir    string       // the app's folder on the card
	Root   string       // where episodes go: the card's Podcasts folder
	Client *http.Client // feeds and the directory: with a total timeout
	Fetch  *http.Client // episodes: no total timeout, stalls watched instead
	Agent  string       // User-Agent
	Ready  func() bool  // downloads may run (Wi-Fi up, the card there); nil checks both
}

type Show struct {
	Key, URL, Title, Folder string
	Auto                    bool
}

// cache is feeds/<key>.json.
type cache struct {
	Title    string            `json:"title"`
	Author   string            `json:"author,omitempty"`
	Image    string            `json:"image,omitempty"`
	Episodes []feed.Episode    `json:"episodes"`
	Names    map[string]string `json:"names,omitempty"` // episode id: its file's name, kept once given
	ETag     string            `json:"etag,omitempty"`
	Modified string            `json:"modified,omitempty"`
	Checked  time.Time         `json:"checked"`
	Err      string            `json:"error,omitempty"`
}

type show struct {
	Show
	c     *cache            // nil until fetched once
	names map[string]string // episode id: its file's name
	index map[string]int    // episode id: its place in c.Episodes
	busy  bool              // a refresh under way
}

// setCache takes a show's feed state, its episodes keeping the names they had.
func (s *show) setCache(c *cache) {
	old := s.names
	if old == nil {
		old = c.Names
	}
	s.c = c
	s.names = fileNames(c.Episodes, old)
	c.Names = s.names
	s.index = make(map[string]int, len(c.Episodes))
	for i, e := range c.Episodes {
		s.index[EpisodeID(e.GUID)] = i
	}
}

type Library struct {
	o      Options
	mu     sync.Mutex
	shows  []*show // by title
	saveMu sync.Mutex
	DL     *Downloader

	fileMu    sync.Mutex // a file put in place (retagged, downloaded) only if not deleted meanwhile
	covers    covers
	retagging atomic.Bool
}

// ShowInfo is a show as the pages show it.
type ShowInfo struct {
	Key, URL, Title, Author, Folder, Image string
	Auto                                   bool
	Episodes, OnCard                       int
	Latest, Checked                        time.Time
	Refreshing, Fetched                    bool
	Err                                    string
}

// EpisodeInfo is an episode as the pages show it.
type EpisodeInfo struct {
	Key, EID, Show, Title string
	Author, Image         string // the show's author; the episode's art, else the show's
	Date                  time.Time
	Duration              int
	Length                int64
	Path                  string // where it is, or would be, on the card
	OnCard                bool
	Size                  int64 // on the card
	Job                   *JobInfo
}

var ErrNoShow = errors.New("that show isn't subscribed")
var ErrNoEpisode = errors.New("that episode isn't in the feed any more")

// Open reads the subscriptions and each show's cached feed. A missing file is an empty library,
// never an error: the card may be fresh. Subscriptions it can't read are kept aside, as
// subscriptions.opml.bad, rather than written over.
func Open(o Options) (*Library, error) {
	for _, d := range []string{o.Dir, filepath.Join(o.Dir, "feeds"), filepath.Join(o.Dir, "partial")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if o.Ready == nil { // Wi-Fi up, and the card there: under USB storage the folder is gone
		o.Ready = func() bool {
			_, err := os.Stat(o.Dir)
			return err == nil && q2.LocalIP() != ""
		}
	}
	l := &Library{o: o}
	var outlines []feed.Outline
	if b, err := os.ReadFile(l.opmlPath()); err == nil {
		if outlines, err = feed.ParseOPML(bytes.NewReader(b)); err != nil {
			os.Rename(l.opmlPath(), l.opmlPath()+".bad")
			log.Printf("can't read the subscriptions (%v): kept as subscriptions.opml.bad", err)
		}
	}
	dirty := false
	for _, ol := range outlines {
		if l.byURL(ol.URL) != nil {
			continue
		}
		s := &show{Show: Show{Key: ol.Key, URL: ol.URL, Title: ol.Title, Folder: ol.Folder, Auto: ol.Auto}}
		if s.Key == "" || l.find(s.Key) != nil { // none, or another show's (a file edited by hand)
			s.Key, dirty = KeyFor(ol.URL), true
			for l.find(s.Key) != nil {
				s.Key = KeyFor(s.Key + ol.URL)
			}
		}
		if s.Title == "" {
			s.Title = ol.URL
		}
		if s.Folder == "" {
			s.Folder, dirty = l.folderFor(s.Title, s.Key), true
		}
		var c cache
		if b, err := os.ReadFile(l.cachePath(s.Key)); err == nil && json.Unmarshal(b, &c) == nil {
			s.setCache(&c)
		}
		l.shows = append(l.shows, s)
	}
	l.sort()
	if dirty {
		l.saveOPML()
	}
	l.DL = newDownloader(l)
	return l, nil
}

func (l *Library) opmlPath() string            { return filepath.Join(l.o.Dir, "subscriptions.opml") }
func (l *Library) cachePath(key string) string { return filepath.Join(l.o.Dir, "feeds", key+".json") }
func (l *Library) partPath(key, eid string) string {
	return filepath.Join(l.o.Dir, "partial", key+"-"+eid+".part")
}

func (l *Library) find(key string) *show {
	for _, s := range l.shows {
		if s.Key == key {
			return s
		}
	}
	return nil
}

func (l *Library) byURL(u string) *show {
	for _, s := range l.shows {
		if s.URL == u {
			return s
		}
	}
	return nil
}

func (l *Library) sort() {
	sort.SliceStable(l.shows, func(i, j int) bool {
		return strings.ToLower(sortTitle(l.shows[i].Title)) < strings.ToLower(sortTitle(l.shows[j].Title))
	})
}

// sortTitle leaves a leading "The " out, as the Q2's Library sorts artists.
func sortTitle(s string) string {
	if len(s) > 4 && strings.EqualFold(s[:4], "the ") {
		return s[4:]
	}
	return s
}

// folderFor is a show's folder: its title made safe, numbered when another show has it (FAT
// ignores case).
func (l *Library) folderFor(title, key string) string {
	base := Safe(title, MaxFolder-5)
	name := base
	for n := 2; ; n++ {
		taken := false
		for _, s := range l.shows {
			if s.Key != key && strings.EqualFold(s.Folder, name) {
				taken = true
			}
		}
		if !taken {
			return name
		}
		name = fmt.Sprintf("%s (%d)", base, n)
	}
}

// outlines is the subscriptions as OPML entries; under l.mu.
func (l *Library) outlines() []feed.Outline {
	out := make([]feed.Outline, 0, len(l.shows))
	for _, s := range l.shows {
		out = append(out, feed.Outline{Title: s.Title, URL: s.URL, Key: s.Key, Folder: s.Folder, Auto: s.Auto})
	}
	return out
}

// OPML is the subscriptions to export.
func (l *Library) OPML() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return feed.WriteOPML("Q2 Podcasts", l.outlines())
}

// saveOPML writes the subscriptions; under l.mu (the write is small).
func (l *Library) saveOPML() error {
	return q2.WriteFile(l.opmlPath(), feed.WriteOPML("Q2 Podcasts", l.outlines()))
}

// saveCache writes a show's feed state outside l.mu: marshalled by the caller under it.
func (l *Library) saveCache(key string, b []byte) {
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	if err := q2.WriteFile(l.cachePath(key), b); err != nil {
		log.Printf("save %s: %v", key, err)
	}
}

func info(s *show) ShowInfo {
	si := ShowInfo{Key: s.Key, URL: s.URL, Title: s.Title, Folder: s.Folder, Auto: s.Auto, Refreshing: s.busy}
	if c := s.c; c != nil {
		si.Fetched = !c.Checked.IsZero()
		si.Author, si.Image, si.Checked, si.Err = c.Author, c.Image, c.Checked, c.Err
		si.Episodes = len(c.Episodes)
		if len(c.Episodes) > 0 {
			si.Latest = c.Episodes[0].Date
		}
	}
	return si
}

// Shows is every subscription, by title, with how many episodes are on the card.
func (l *Library) Shows() []ShowInfo {
	l.mu.Lock()
	out := make([]ShowInfo, 0, len(l.shows))
	names := make([]map[string]string, 0, len(l.shows))
	for _, s := range l.shows {
		out = append(out, info(s))
		names = append(names, s.names)
	}
	l.mu.Unlock()
	for i := range out {
		on := l.onCard(out[i].Folder)
		for _, n := range names[i] {
			if _, ok := on[strings.ToLower(n)]; ok {
				out[i].OnCard++
			}
		}
	}
	return out
}

// Show is one subscription.
func (l *Library) Show(key string) (ShowInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.find(key); s != nil {
		return info(s), true
	}
	return ShowInfo{}, false
}

func (l *Library) episode(s *show, e feed.Episode) EpisodeInfo {
	eid := EpisodeID(e.GUID)
	return EpisodeInfo{Key: s.Key, EID: eid, Show: s.Title, Title: e.Title, Date: e.Date, Duration: e.Duration,
		Length: e.Length, Path: filepath.Join(l.o.Root, s.Folder, s.names[eid]), Author: s.c.Author,
		Image: cmp.Or(e.Image, s.c.Image)}
}

// onCard is a folder's files by lower-case name; empty when it isn't there. Sizes are looked up
// (sizeOf) only for the files wanted: each is a stat on the card.
func (l *Library) onCard(folder string) map[string]os.DirEntry {
	out := map[string]os.DirEntry{}
	ents, _ := os.ReadDir(filepath.Join(l.o.Root, folder))
	for _, e := range ents {
		if e.Type().IsRegular() {
			out[strings.ToLower(e.Name())] = e
		}
	}
	return out
}

func sizeOf(e os.DirEntry) int64 {
	if fi, err := e.Info(); err == nil {
		return fi.Size()
	}
	return 0
}

// mark fills in which episodes are on the card (each folder read once) and their downloads.
func (l *Library) mark(eps []EpisodeInfo) []EpisodeInfo {
	folders := map[string]map[string]os.DirEntry{}
	for i := range eps {
		dir := filepath.Dir(eps[i].Path)
		on, ok := folders[dir]
		if !ok {
			on = l.onCard(filepath.Base(dir))
			folders[dir] = on
		}
		if f, ok := on[strings.ToLower(filepath.Base(eps[i].Path))]; ok {
			eps[i].OnCard, eps[i].Size = true, sizeOf(f)
		} else if l.DL != nil { // nil while the queue itself is read back
			eps[i].Job = l.DL.Job(eps[i].Key, eps[i].EID)
		}
	}
	return eps
}

// Episodes is a show's episodes, newest first.
func (l *Library) Episodes(key string) ([]EpisodeInfo, error) {
	l.mu.Lock()
	s := l.find(key)
	if s == nil {
		l.mu.Unlock()
		return nil, ErrNoShow
	}
	var out []EpisodeInfo
	if s.c != nil {
		out = make([]EpisodeInfo, 0, len(s.c.Episodes))
		for _, e := range s.c.Episodes {
			out = append(out, l.episode(s, e))
		}
	}
	l.mu.Unlock()
	return l.mark(out), nil
}

// Episode is one episode.
func (l *Library) Episode(key, eid string) (EpisodeInfo, error) {
	l.mu.Lock()
	s := l.find(key)
	if s == nil {
		l.mu.Unlock()
		return EpisodeInfo{}, ErrNoShow
	}
	i, ok := s.index[eid]
	if !ok {
		l.mu.Unlock()
		return EpisodeInfo{}, ErrNoEpisode
	}
	e := l.episode(s, s.c.Episodes[i])
	l.mu.Unlock()
	return l.mark([]EpisodeInfo{e})[0], nil
}

// source is where an episode's audio is fetched from.
func (l *Library) source(key, eid string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.find(key)
	if s == nil {
		return "", ErrNoShow
	}
	i, ok := s.index[eid]
	if !ok {
		return "", ErrNoEpisode
	}
	return s.c.Episodes[i].URL, nil
}

// Latest is the newest n episodes of all the shows.
func (l *Library) Latest(n int) []EpisodeInfo {
	l.mu.Lock()
	var out []EpisodeInfo
	for _, s := range l.shows {
		if s.c == nil {
			continue
		}
		for i, e := range s.c.Episodes {
			if i == n {
				break
			}
			out = append(out, l.episode(s, e))
		}
	}
	l.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	if len(out) > n {
		out = out[:n]
	}
	return l.mark(out)
}

// Downloaded is every episode on the card, newest first, and their bytes.
func (l *Library) Downloaded() ([]EpisodeInfo, int64) {
	l.mu.Lock()
	folders := make([]string, 0, len(l.shows))
	for _, s := range l.shows {
		folders = append(folders, s.Folder)
	}
	l.mu.Unlock()
	on := make(map[string]map[string]os.DirEntry, len(folders)) // the card read outside l.mu
	for _, f := range folders {
		on[f] = l.onCard(f)
	}
	l.mu.Lock()
	var out []EpisodeInfo
	var files []os.DirEntry
	for _, s := range l.shows {
		if s.c == nil || len(on[s.Folder]) == 0 {
			continue
		}
		for eid, name := range s.names {
			if f, ok := on[s.Folder][strings.ToLower(name)]; ok {
				out = append(out, l.episode(s, s.c.Episodes[s.index[eid]]))
				files = append(files, f)
			}
		}
	}
	l.mu.Unlock()
	var bytes int64
	for i := range out {
		out[i].OnCard, out[i].Size = true, sizeOf(files[i])
		bytes += out[i].Size
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.After(out[j].Date)
		}
		return out[i].Path < out[j].Path
	})
	return out, bytes
}

// Normalize makes what someone typed or pasted a feed URL: podcast schemes (feed:, itpc:,
// pcast:, podcast:) as HTTP, https:// added to a bare address.
func Normalize(raw string) (string, error) {
	s := strings.Trim(strings.TrimSpace(raw), "<>\"'")
	low := strings.ToLower(s)
	for _, p := range []string{"feed:", "itpc:", "pcast:", "podcast:"} {
		if strings.HasPrefix(low, p) {
			s = strings.TrimPrefix(s[len(p):], "//")
			if !strings.Contains(strings.ToLower(s), "://") {
				s = "http://" + s
			}
			low = strings.ToLower(s)
			break
		}
	}
	if !strings.Contains(low, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !hostname.MatchString(u.Hostname()) {
		return "", errors.New("that isn't a web address")
	}
	return u.String(), nil
}

// Host is an address's host, for the log and the screen: the rest may hold a paid feed's key.
func Host(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return "?"
}

// hostname is a name or an IPv4 address: the Q2 has no IPv6.
var hostname = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// LooksLikeURL is whether a search box's text is an address rather than a name to search for.
func LooksLikeURL(s string) bool {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") || strings.HasPrefix(strings.ToLower(s), "www.") {
		return true
	}
	return !strings.ContainsAny(s, " ") && strings.Contains(s, ".") && strings.Contains(s, "/")
}

// Fetched is a feed as read, with what makes its next fetch conditional.
type Fetched struct {
	Feed           *feed.Feed
	URL            string // the feed's, after discovery
	ETag, Modified string
	unchanged      bool
}

var alternate = regexp.MustCompile(`(?is)<link\b[^>]*\btype\s*=\s*["']application/(?:rss|atom)\+xml["'][^>]*>`)
var hrefAttr = regexp.MustCompile(`(?is)\bhref\s*=\s*["']([^"']+)["']`)

// discover is the feed a web page names in its <link rel="alternate" type="application/rss+xml">.
func discover(page []byte, base *url.URL) string {
	for _, tag := range alternate.FindAll(page, -1) {
		if m := hrefAttr.FindSubmatch(tag); m != nil {
			if u, err := base.Parse(strings.TrimSpace(string(m[1]))); err == nil {
				return u.String()
			}
		}
	}
	return ""
}

// netErr words a failed request for the Q2's screen.
func netErr(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return errors.New("the feed took too long to answer")
	}
	return errors.New("can't reach the feed: check the Wi-Fi")
}

func statusErr(code int) error {
	switch {
	case code == 404 || code == 410:
		return fmt.Errorf("the feed isn't there any more (HTTP %d)", code)
	case code == 401 || code == 403:
		return fmt.Errorf("the feed needs a sign-in the Q2 doesn't have (HTTP %d)", code)
	case code == 429:
		return errors.New("the feed's server is busy: try again later")
	}
	return fmt.Errorf("the feed's server failed (HTTP %d)", code)
}

// fetch gets and reads a feed, conditionally when etag or modified is set. A web page that names
// its feed is followed to it, once (discover).
func (l *Library) fetch(ctx context.Context, u, etag, modified string, discoverOK bool) (*Fetched, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("that isn't a web address")
	}
	req.Header.Set("User-Agent", l.o.Agent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	resp, err := l.o.Client.Do(req)
	if err != nil {
		return nil, netErr(err)
	}
	defer resp.Body.Close()
	out := &Fetched{URL: u, ETag: resp.Header.Get("ETag"), Modified: resp.Header.Get("Last-Modified")}
	if resp.StatusCode == http.StatusNotModified && (etag != "" || modified != "") {
		out.unchanged = true
		return out, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp.StatusCode)
	}
	var head bytes.Buffer // the start, for discovery should it be a web page
	cut := &cutShort{r: io.LimitReader(resp.Body, MaxFeed)}
	f, err := feed.Parse(io.TeeReader(cut, &capped{&head, 256 << 10}))
	if cut.err != nil || cut.n >= MaxFeed { // the episodes read stand; the next fetch is in full
		out.ETag, out.Modified = "", ""
	}
	if errors.Is(err, feed.ErrNotFeed) && discoverOK {
		if next := discover(head.Bytes(), resp.Request.URL); next != "" && next != u {
			return l.fetch(ctx, next, "", "", false)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, netErr(ctx.Err())
		}
		if errors.Is(err, feed.ErrNotFeed) {
			return nil, errors.New("that address isn't a podcast feed")
		}
		return nil, errors.New("the feed is broken: " + err.Error())
	}
	out.Feed = f
	return out, nil
}

// cutShort notes a read that failed (a dropped connection) and how much was read.
type cutShort struct {
	r   io.Reader
	n   int64
	err error
}

func (c *cutShort) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && err != io.EOF && c.err == nil {
		c.err = err
	}
	return n, err
}

type capped struct {
	b   *bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// Resolve is the feed URL behind what someone typed or pasted: an address, or an Apple Podcasts
// link (looked up).
func (l *Library) Resolve(ctx context.Context, raw string) (string, error) {
	if id := directory.AppleID(raw); id != "" {
		s, err := directory.Lookup(ctx, l.o.Client, id)
		if err != nil {
			return "", err
		}
		return s.FeedURL, nil
	}
	return Normalize(raw)
}

// Preview fetches a feed without subscribing; its URL is the feed's, after discovery.
func (l *Library) Preview(ctx context.Context, u string) (*Fetched, error) {
	return l.fetch(ctx, u, "", "", true)
}

// Subscribed is the show subscribed at this feed URL.
func (l *Library) Subscribed(u string) (ShowInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := l.byURL(u); s != nil {
		return info(s), true
	}
	return ShowInfo{}, false
}

// Subscribe adds the show at raw (an address or an Apple Podcasts link), fetched first so only a
// real feed is added. Already subscribed, it is that show.
func (l *Library) Subscribe(ctx context.Context, raw string) (ShowInfo, error) {
	u, err := l.Resolve(ctx, raw)
	if err != nil {
		return ShowInfo{}, err
	}
	if si, ok := l.Subscribed(u); ok {
		return si, nil
	}
	r, err := l.Preview(ctx, u)
	if err != nil {
		return ShowInfo{}, err
	}
	return l.Adopt(r), nil
}

func cacheOf(f *feed.Feed, etag, modified string) *cache {
	eps := f.Episodes
	if len(eps) > MaxEpisodes {
		eps = append([]feed.Episode(nil), eps[:MaxEpisodes]...) // the rest let go
	}
	return &cache{Title: f.Title, Author: f.Author, Image: f.Image, Episodes: eps,
		ETag: etag, Modified: modified, Checked: time.Now()}
}

// Adopt subscribes to a feed already fetched (Preview's).
func (l *Library) Adopt(r *Fetched) ShowInfo {
	u, f := r.URL, r.Feed
	l.mu.Lock()
	if s := l.byURL(u); s != nil {
		defer l.mu.Unlock()
		return info(s)
	}
	key := KeyFor(u)
	for l.find(key) != nil { // the same URL's hash was taken by a show since moved
		key = KeyFor(key + u)
	}
	title := f.Title
	if title == "" {
		title = "Untitled podcast"
	}
	s := &show{Show: Show{Key: key, URL: u, Title: title}}
	s.Folder = l.folderFor(title, key)
	s.setCache(cacheOf(f, r.ETag, r.Modified))
	l.shows = append(l.shows, s)
	l.sort()
	l.saveOPML()
	b, _ := json.Marshal(s.c)
	si := info(s)
	l.mu.Unlock()
	l.saveCache(key, b)
	log.Printf("subscribed to %s (%s)", s.Title, Host(u)) // a paid feed's address holds its key
	return si
}

// Import adds the outlines not already subscribed, without fetching them (a refresh does); how
// many were added.
func (l *Library) Import(outlines []feed.Outline) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ol := range outlines {
		u, err := Normalize(ol.URL)
		if err != nil || l.byURL(u) != nil {
			continue
		}
		key := KeyFor(u)
		for l.find(key) != nil {
			key = KeyFor(key + u)
		}
		title := ol.Title
		if title == "" {
			title = u
		}
		s := &show{Show: Show{Key: key, URL: u, Title: title, Auto: ol.Auto}}
		s.Folder = l.folderFor(title, key)
		l.shows = append(l.shows, s)
		n++
	}
	if n > 0 {
		l.sort()
		l.saveOPML()
	}
	return n
}

// Unsubscribe removes a show and its waiting downloads. Its episodes stay on the card.
func (l *Library) Unsubscribe(key string) error {
	l.mu.Lock()
	i := -1
	for k, s := range l.shows {
		if s.Key == key {
			i = k
		}
	}
	if i < 0 {
		l.mu.Unlock()
		return ErrNoShow
	}
	title := l.shows[i].Title
	l.shows = append(l.shows[:i], l.shows[i+1:]...)
	err := l.saveOPML()
	l.mu.Unlock()
	l.DL.CancelShow(key)
	os.Remove(l.cachePath(key))
	log.Printf("unsubscribed from %s", title)
	return err
}

// SetAuto turns a show's automatic downloads of new episodes on or off.
func (l *Library) SetAuto(key string, on bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.find(key)
	if s == nil {
		return ErrNoShow
	}
	s.Auto = on
	return l.saveOPML()
}

// Refresh fetches a show's feed again (conditionally), keeping episodes the feed no longer lists
// up to MaxEpisodes, and queues new episodes when the show downloads them by itself. A refresh
// already under way is left to finish.
func (l *Library) Refresh(ctx context.Context, key string) error { return l.refresh(ctx, key, false) }

// refresh is Refresh, in full (not conditionally) when full: a feed whose links are signed as it is
// fetched may answer "not modified" with its old, expired ones.
func (l *Library) refresh(ctx context.Context, key string, full bool) error {
	l.mu.Lock()
	s := l.find(key)
	if s == nil {
		l.mu.Unlock()
		return ErrNoShow
	}
	if s.busy {
		l.mu.Unlock()
		return nil
	}
	s.busy = true
	u, prev := s.URL, s.c
	etag, modified := "", ""
	if prev != nil && len(prev.Episodes) > 0 && !full {
		etag, modified = prev.ETag, prev.Modified
	}
	l.mu.Unlock()

	r, err := l.fetch(ctx, u, etag, modified, prev == nil)

	l.mu.Lock()
	s.busy = false
	if l.find(key) != s { // unsubscribed meanwhile
		l.mu.Unlock()
		return ErrNoShow
	}
	c := &cache{}
	if prev != nil {
		*c = *prev
	}
	var fresh []string
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) {
			l.mu.Unlock()
			return err
		}
		c.Err = err.Error()
	case r.unchanged:
		c.Checked, c.Err = time.Now(), ""
	default:
		nc := cacheOf(r.Feed, r.ETag, r.Modified)
		if prev != nil {
			renamed(prev.Episodes, nc.Episodes)
			known := make(map[string]bool, len(prev.Episodes))
			for _, e := range prev.Episodes {
				known[e.GUID] = true
			}
			seen := make(map[string]bool, len(nc.Episodes))
			for _, e := range nc.Episodes {
				seen[e.GUID] = true
				if !known[e.GUID] && len(fresh) < autoMax {
					fresh = append(fresh, EpisodeID(e.GUID))
				}
			}
			for _, e := range prev.Episodes { // ones the feed has dropped, kept
				if !seen[e.GUID] && len(nc.Episodes) < MaxEpisodes {
					nc.Episodes = append(nc.Episodes, e)
				}
			}
			sort.SliceStable(nc.Episodes, func(i, j int) bool { return nc.Episodes[i].Date.After(nc.Episodes[j].Date) })
		}
		c = nc
		dirty := false
		if r.Feed.NewURL != "" && r.Feed.NewURL != s.URL {
			if nu, err := Normalize(r.Feed.NewURL); err == nil && l.byURL(nu) == nil {
				log.Printf("%s moved to %s", s.Title, Host(nu))
				s.URL, dirty = nu, true
			}
		} else if r.URL != s.URL && prev == nil { // a web page's feed, found
			s.URL, dirty = r.URL, true
		}
		if r.Feed.Title != "" && r.Feed.Title != s.Title {
			s.Title, dirty = r.Feed.Title, true
			l.sort()
		}
		if dirty {
			l.saveOPML()
		}
	}
	s.setCache(c)
	auto := s.Auto && prev != nil && len(prev.Episodes) > 0
	b, _ := json.Marshal(c)
	l.mu.Unlock()
	l.saveCache(key, b)
	if auto {
		for _, eid := range fresh {
			l.DL.Queue(key, eid)
		}
	}
	return err
}

// renamed gives an episode the id it had when a version before named it differently (by its whole
// link where it has no guid; one of several sharing a guid by the guid alone): the same link and
// title, its old id no longer in the feed.
func renamed(prev, eps []feed.Episode) {
	ids := make(map[string]bool, len(eps))
	for _, e := range eps {
		ids[e.GUID] = true
	}
	was := map[[2]string]string{}
	for _, e := range prev {
		if !ids[e.GUID] {
			was[[2]string{e.URL, e.Title}] = e.GUID
		}
	}
	for i, e := range eps {
		k := [2]string{e.URL, e.Title}
		if id, ok := was[k]; ok {
			eps[i].GUID = id
			delete(was, k)
		}
	}
}

// RefreshAll refreshes, two at a time, every show not fetched within stale; how many it tried.
func (l *Library) RefreshAll(ctx context.Context, stale time.Duration) int {
	l.mu.Lock()
	var keys []string
	for _, s := range l.shows {
		if s.c == nil || outdated(s.c.Checked, stale) || s.c.Err != "" && stale > 0 {
			keys = append(keys, s.Key)
		}
	}
	l.mu.Unlock()
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range work {
				l.Refresh(ctx, k)
			}
		}()
	}
	for _, k := range keys {
		select {
		case work <- k:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	return len(keys)
}

// outdated is whether a feed checked at t is older than stale, or checked "later" than now: the Q2's
// clock set back (its battery flat) would otherwise hold refreshes off until it caught up.
func outdated(t time.Time, stale time.Duration) bool {
	d := time.Since(t)
	return d < 0 || d > stale
}

// Refreshing is how many shows are being fetched.
func (l *Library) Refreshing() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.shows {
		if s.busy {
			n++
		}
	}
	return n
}

// Delete removes an episode from the card (or its download), and its show's folder once empty.
func (l *Library) Delete(key, eid string) error {
	l.DL.Cancel(key, eid)
	e, err := l.Episode(key, eid)
	if err != nil {
		return err
	}
	l.fileMu.Lock()
	err = os.Remove(e.Path)
	l.fileMu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	os.Remove(filepath.Dir(e.Path)) // only when empty
	return nil
}

// DeleteAll removes every episode of a show from the card; how many, and their bytes.
func (l *Library) DeleteAll(key string) (int, int64, error) {
	eps, err := l.Episodes(key)
	if err != nil {
		return 0, 0, err
	}
	l.DL.CancelShow(key)
	n, size := 0, int64(0)
	l.fileMu.Lock()
	for _, e := range eps {
		if os.Remove(e.Path) == nil { // one that landed before the cancel too (its size not known)
			n++
			size += e.Size
		}
	}
	l.fileMu.Unlock()
	if len(eps) > 0 {
		os.Remove(filepath.Dir(eps[0].Path))
	}
	return n, size, nil
}
