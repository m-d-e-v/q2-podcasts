package library

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"q2-podcasts/internal/q2"
)

const (
	chunk    = 512 << 10        // written to the card at a time, each opened, synced and closed
	stallFor = 60 * time.Second // no bytes this long: the transfer is given up and tried again
	reserve  = 32 << 20         // the card keeps this much free
	tries    = 4                // a download that fails this often (but for the network) is failed
	idleWait = 30 * time.Second
	renewAge = 5 * time.Minute // a feed older than this is fetched again for a refused episode's address
)

// retryAfter is the wait before a failed download's second try, doubling each time; tests shorten it.
var retryAfter = 10 * time.Second

type JobState int

const (
	Waiting JobState = iota
	Active
	Failed
)

// JobInfo is a download as the pages show it.
type JobInfo struct {
	Key, EID, Show, Title string
	State                 JobState
	Err                   string
	Done, Size            int64 // bytes; Size -1 when unknown
}

type job struct {
	JobInfo
	tries     int
	notBefore time.Time
	discard   bool // cancelled: its .part goes too
}

// queued is a download as queue.json keeps it.
type queued struct {
	Key, EID string
	Failed   bool   `json:",omitempty"`
	Err      string `json:",omitempty"`
}

// Downloader fetches episodes onto the card one at a time, in the order asked.
type Downloader struct {
	l       *Library
	mu      sync.Mutex
	jobs    []*job
	cancel  context.CancelFunc // the active job's
	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	started bool
}

func newDownloader(l *Library) *Downloader {
	d := &Downloader{l: l, wake: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{})}
	var saved []queued
	if b, err := os.ReadFile(d.path()); err == nil {
		json.Unmarshal(b, &saved)
	}
	for _, s := range saved {
		if e, err := l.Episode(s.Key, s.EID); err == nil && !e.OnCard {
			j := &job{JobInfo: JobInfo{Key: s.Key, EID: s.EID, Show: e.Show, Title: e.Title, Size: e.Length, Err: s.Err}}
			if s.Failed {
				j.State = Failed
			}
			d.jobs = append(d.jobs, j)
		}
	}
	return d
}

func (d *Downloader) path() string { return filepath.Join(d.l.o.Dir, "queue.json") }

// save writes the queue; under d.mu.
func (d *Downloader) save() {
	out := []queued{}
	for _, j := range d.jobs {
		if !j.discard {
			out = append(out, queued{j.Key, j.EID, j.State == Failed, j.Err})
		}
	}
	b, _ := json.Marshal(out)
	if err := q2.WriteFile(d.path(), b); err != nil {
		log.Printf("save queue: %v", err)
	}
}

func (d *Downloader) poke() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Start runs the queue until Stop.
func (d *Downloader) Start() {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()
	go d.run()
}

// Stop ends the download under way, keeping its .part to resume, and waits for it.
func (d *Downloader) Stop() {
	d.mu.Lock()
	started := d.started
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	if started {
		<-d.stopped
	}
}

// find is an episode's download; one cancelled, on its way out, isn't.
func (d *Downloader) find(key, eid string) (int, *job) {
	for i, j := range d.jobs {
		if j.Key == key && j.EID == eid && !j.discard {
			return i, j
		}
	}
	return -1, nil
}

// Queue asks for an episode; one already waiting stays where it is, a failed one is tried again.
func (d *Downloader) Queue(key, eid string) (string, error) {
	e, err := d.l.Episode(key, eid)
	if err != nil {
		return "", err
	}
	if e.OnCard {
		return "It's already on the Q2", nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, j := d.find(key, eid); j != nil {
		if j.State == Failed {
			j.State, j.Err, j.tries, j.notBefore = Waiting, "", 0, time.Time{}
			d.save()
			d.poke()
			return "Trying again", nil
		}
		return "It's already downloading", nil
	}
	ahead := 0
	for _, j := range d.jobs {
		if !j.discard {
			ahead++
		}
	}
	d.jobs = append(d.jobs, &job{JobInfo: JobInfo{Key: key, EID: eid, Show: e.Show, Title: e.Title, Size: e.Length}})
	d.save()
	d.poke()
	if ahead == 0 {
		return "Downloading", nil
	}
	return fmt.Sprintf("Downloading after %d more", ahead), nil
}

// drop removes job i, its transfer stopped and its .part deleted; under d.mu.
func (d *Downloader) drop(i int) {
	j := d.jobs[i]
	if j.State == Active {
		j.discard = true // the transfer's end removes it and its .part
		if d.cancel != nil {
			d.cancel()
		}
		return
	}
	d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
	removePart(d.l.partPath(j.Key, j.EID))
}

// Cancel drops an episode's download; false when there was none.
func (d *Downloader) Cancel(key, eid string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, j := d.find(key, eid)
	if j == nil {
		return false
	}
	d.drop(i)
	d.save()
	return true
}

// CancelShow drops a show's downloads.
func (d *Downloader) CancelShow(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.jobs) - 1; i >= 0; i-- {
		if d.jobs[i].Key == key {
			d.drop(i)
		}
	}
	d.save()
}

// CancelAll drops every download.
func (d *Downloader) CancelAll() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.jobs)
	for i := len(d.jobs) - 1; i >= 0; i-- {
		d.drop(i)
	}
	d.save()
	return n
}

// RetryFailed tries every failed download again; how many.
func (d *Downloader) RetryFailed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, j := range d.jobs {
		if j.State == Failed {
			j.State, j.Err, j.tries, j.notBefore = Waiting, "", 0, time.Time{}
			n++
		}
	}
	if n > 0 {
		d.save()
		d.poke()
	}
	return n
}

// Job is an episode's download, nil when there is none.
func (d *Downloader) Job(key, eid string) *JobInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, j := d.find(key, eid); j != nil {
		ji := j.JobInfo
		return &ji
	}
	return nil
}

// Jobs is the queue, in order.
func (d *Downloader) Jobs() []JobInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]JobInfo, 0, len(d.jobs))
	for _, j := range d.jobs {
		if !j.discard {
			out = append(out, j.JobInfo)
		}
	}
	return out
}

// Busy is whether a download is moving bytes now.
func (d *Downloader) Busy() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State == Active {
			return true
		}
	}
	return false
}

// Pending is whether a download waits that could run now.
func (d *Downloader) Pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State != Failed {
			return true
		}
	}
	return false
}

// next is the first waiting job due, marked active with its cancel; or nil and how long until one
// waiting to try again is due (idleWait at most).
func (d *Downloader) next() (*job, context.Context, time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	select {
	case <-d.stop: // Stop came after run looked: nothing starts that it won't cancel
		return nil, nil, idleWait
	default:
	}
	now, wait := time.Now(), idleWait
	for _, j := range d.jobs {
		if j.State != Waiting {
			continue
		}
		if due := j.notBefore.Sub(now); due > 0 {
			wait = min(wait, due)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		j.State, j.Err, j.Done = Active, "", 0
		d.cancel = cancel
		return j, ctx, 0
	}
	return nil, nil, wait
}

func (d *Downloader) run() {
	defer close(d.stopped)
	for {
		select {
		case <-d.stop:
			return
		default:
		}
		var j *job
		var ctx context.Context
		wait := idleWait
		if d.l.o.Ready() {
			j, ctx, wait = d.next()
		}
		if j == nil {
			select {
			case <-d.stop:
				return
			case <-d.wake:
			case <-time.After(wait):
			}
			continue
		}
		err := d.l.download(ctx, j, func(done, size int64) {
			d.mu.Lock()
			j.Done, j.Size = done, size
			d.mu.Unlock()
		})
		d.finish(j, err)
	}
}

// permanent is a failure trying again won't mend.
type permanent struct{ error }

// refused is a server's no to an episode's address, which may be a paid feed's signed link that has
// expired: the feed fetched again has a fresh one.
type refused struct{ code int }

func (r refused) Error() string {
	if r.code == http.StatusNotFound || r.code == http.StatusGone {
		return fmt.Sprintf("the episode isn't there any more (HTTP %d)", r.code)
	}
	return fmt.Sprintf("the episode's server refused (HTTP %d)", r.code)
}

func (d *Downloader) finish(j *job, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancel = nil
	i := slices.Index(d.jobs, j) // not by its episode: a cancelled one may be asked for again
	stopping := false
	select {
	case <-d.stop:
		stopping = true
	default:
	}
	switch {
	case j.discard:
		if i >= 0 {
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
		}
		removePart(d.l.partPath(j.Key, j.EID))
		log.Printf("cancelled %s", j.Title)
	case err == nil:
		if i >= 0 {
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
		}
		log.Printf("downloaded %s: %s", j.Show, j.Title)
	case stopping:
		j.State = Waiting // resumed from its .part next time
	case errors.Is(err, q2.ErrNoCard): // USB storage took it: no try spent, Q2 Pod restarts the app after
		j.State, j.Err, j.notBefore = Waiting, "", time.Now().Add(idleWait)
	default:
		var p permanent
		if j.tries++; errors.As(err, &p) || j.tries >= tries {
			j.State, j.Err = Failed, err.Error()
		} else {
			j.State, j.Err = Waiting, err.Error()
			j.notBefore = time.Now().Add(retryAfter << (j.tries - 1))
		}
		log.Printf("download %s: %v (try %d)", j.Title, err, j.tries)
	}
	d.save()
}

// freeBytes is the room left on the file system holding path; tests stand in for it.
var freeBytes = func(path string) (int64, bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, false
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), true
}

// download fetches one episode to its .part, resuming what is there, and renames it into its
// show's folder once whole. An address refused is tried once more, fresh from the feed.
func (l *Library) download(ctx context.Context, j *job, progress func(done, size int64)) error {
	e, err := l.Episode(j.Key, j.EID)
	if err != nil {
		return permanent{err}
	}
	if e.OnCard {
		return nil
	}
	u, err := l.source(j.Key, j.EID)
	if err != nil {
		return permanent{err}
	}
	err = l.transfer(ctx, u, e, l.partPath(j.Key, j.EID), progress)
	var no refused
	if errors.As(err, &no) {
		if fresh := l.renewSource(ctx, j.Key, j.EID, u); fresh != "" {
			err = l.transfer(ctx, fresh, e, l.partPath(j.Key, j.EID), progress)
		}
	}
	if errors.As(err, &no) {
		return permanent{err}
	}
	return err
}

// renewSource fetches a show's feed again, in full, for an episode's address when the feed is older
// than renewAge; "" when that brings no new address.
func (l *Library) renewSource(ctx context.Context, key, eid, old string) string {
	l.mu.Lock()
	s := l.find(key)
	stale := s != nil && s.c != nil && outdated(s.c.Checked, renewAge)
	l.mu.Unlock()
	if !stale || l.refresh(ctx, key, true) != nil {
		return ""
	}
	if u, err := l.source(key, eid); err == nil && u != old {
		log.Printf("a new address for %s", eid)
		return u
	}
	return ""
}

// partInfo is part+".json": the file a .part is of, so a resume is only ever of the same file (an
// episode with ads put in as it is served can change between tries).
type partInfo struct {
	Size           int64  `json:"size"` // the whole file's; -1 unknown
	ETag, Modified string `json:",omitempty"`
}

func removePart(part string) {
	os.Remove(part)
	os.Remove(part + ".json")
}

// transfer fetches an episode from u to part, resuming what is there, and renames it into place.
func (l *Library) transfer(ctx context.Context, u string, e EpisodeInfo, part string, progress func(done, size int64)) error {
	var off int64
	var was partInfo
	if st, err := os.Stat(part); err == nil {
		if b, err := os.ReadFile(part + ".json"); err == nil && json.Unmarshal(b, &was) == nil {
			off = st.Size()
		}
	}
	if was.Size > 0 && off == was.Size { // whole already: only its placing failed
		return l.place(ctx, part, e)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return permanent{errors.New("the episode's address is broken")}
	}
	req.Header.Set("User-Agent", l.o.Agent)
	if off > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
		if v := cmp.Or(was.ETag, was.Modified); v != "" {
			req.Header.Set("If-Range", v) // changed since, it comes whole
		}
	}
	resp, err := l.o.Fetch.Do(req)
	if err != nil {
		return errors.New("can't reach the episode: check the Wi-Fi")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, total := contentRange(resp.Header.Get("Content-Range"))
		if start != off {
			removePart(part)
			return fmt.Errorf("the server resumed at the wrong place")
		}
		if total >= 0 && was.Size >= 0 && total != was.Size {
			removePart(part)
			return errors.New("the episode changed: starting again")
		}
	case http.StatusOK:
		off = 0
		removePart(part)
		info, _ := json.Marshal(partInfo{Size: resp.ContentLength, ETag: resp.Header.Get("ETag"), Modified: resp.Header.Get("Last-Modified")})
		if err := os.MkdirAll(filepath.Dir(part), 0o755); err == nil {
			os.WriteFile(part+".json", info, 0o644)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		removePart(part)
		return errors.New("the episode changed: starting again")
	case http.StatusNotFound, http.StatusGone, http.StatusUnauthorized, http.StatusForbidden:
		return refused{resp.StatusCode}
	default:
		return fmt.Errorf("the episode's server failed (HTTP %d)", resp.StatusCode)
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.HasPrefix(ct, "text/html") {
		return permanent{errors.New("the episode's link is a web page, not audio")}
	}
	size := int64(-1)
	if resp.ContentLength >= 0 {
		size = off + resp.ContentLength
	}
	need := size - off
	if need < 0 {
		need = max(e.Length, 0)
	}
	if free, ok := freeBytes(l.o.Dir); ok && free < need+reserve {
		return permanent{errors.New("the card is full")}
	}
	progress(off, size)

	// Bytes arrive into buf; each full buf goes to the card. A stall cancels the request (fetching),
	// not the placing after.
	fetching, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	stall := time.AfterFunc(stallFor, func() { stalled.Store(true); cancel() })
	defer stall.Stop()
	go func() { <-fetching.Done(); resp.Body.Close() }()
	buf := make([]byte, chunk)
	n := 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		if free, ok := freeBytes(l.o.Dir); ok && free < int64(n)+reserve { // a length not known ahead, say
			return permanent{errors.New("the card is full")}
		}
		if err := q2.Append(part, off, buf[:n]); err != nil {
			if errors.Is(err, q2.ErrNoCard) {
				return err
			}
			if free, ok := freeBytes(l.o.Dir); ok && free < chunk {
				return permanent{errors.New("the card is full")}
			}
			return fmt.Errorf("can't write to the card: %v", err)
		}
		off += int64(n)
		n = 0
		progress(off, size)
		return nil
	}
	for {
		k, rerr := resp.Body.Read(buf[n:])
		if k > 0 {
			stall.Reset(stallFor)
			if n += k; n == len(buf) {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if err := flush(); err != nil { // what arrived is good: kept for the resume
				return err
			}
			if stalled.Load() {
				return errors.New("the download stalled")
			}
			if ctx.Err() != nil {
				return ctx.Err() // cancelled, or the app stopping
			}
			return errors.New("the download was cut off")
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if size >= 0 && off != size {
		return errors.New("the download was cut off")
	}
	if off < 1024 {
		removePart(part)
		return permanent{errors.New("the episode's file is empty")}
	}
	stall.Stop() // tagging a long episode is no stall
	return l.place(ctx, part, e)
}

// contentRange is the first byte a Content-Range ("bytes 100-199/200") covers and the whole file's
// size, each -1 when unreadable (or "*").
func contentRange(h string) (start, total int64) {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "bytes"))
	start, total = -1, -1
	if i := strings.IndexByte(h, '-'); i > 0 {
		if v, err := strconv.ParseInt(strings.TrimSpace(h[:i]), 10, 64); err == nil {
			start = v
		}
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		if v, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64); err == nil {
			total = v
		}
	}
	return start, total
}
