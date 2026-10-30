package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"q2-podcasts/internal/tag"
)

// The Q2's Now Playing shows a file's own tags, and a feed's MP3s often have none (Patreon's): an
// episode is tagged as it lands on the card (place), and those that came before without tags are
// tagged in the background (Retag). Either way the tagged file is a copy, its audio as it came, put
// in place by a rename.

var errTagged = errors.New("tagged already")

// covers keeps the covers fetched lately, by address: an episode's file and the next of its show
// share one. nil when it couldn't be had, so it isn't asked for again this run.
type covers struct {
	mu    sync.Mutex
	got   map[string][]byte
	order []string
}

const coversKept = 16

func (l *Library) cover(ctx context.Context, u string) []byte {
	if u == "" {
		return nil
	}
	c := &l.covers
	c.mu.Lock()
	b, ok := c.got[u]
	c.mu.Unlock()
	if ok {
		return b
	}
	b = tag.Cover(ctx, l.o.Client, u, l.o.Agent)
	if ctx.Err() != nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.got == nil {
		c.got = map[string][]byte{}
	}
	if len(c.order) == coversKept {
		delete(c.got, c.order[0])
		c.order = c.order[1:]
	}
	c.got[u] = b
	c.order = append(c.order, u)
	return b
}

func isMP3(path string) bool { return strings.EqualFold(filepath.Ext(path), ".mp3") }

// tmpOf is where path's tagged copy is written: hidden beside it, out of the Q2's lists.
func tmpOf(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tag")
}

// tagTo writes src, tagged for e, to dst: errTagged when its tag has all it needs already (or is
// too big to keep), tag.ErrNotMP3 when it isn't one. A panic reading a file (a tag broken some way
// not foreseen) is an error, not the app's end: Retag would meet the file again at each start.
func (l *Library) tagTo(ctx context.Context, src, dst string, e EpisodeInfo) (err error) {
	defer func() {
		if p := recover(); p != nil {
			os.Remove(dst)
			err = fmt.Errorf("can't read its tag: %v", p)
		}
	}()
	h, err := tag.Read(src)
	if err != nil {
		return err
	}
	if h.Big() || !h.Needs(false) && h.HasCover() {
		return errTagged
	}
	var cover []byte
	if !h.HasCover() {
		cover = l.cover(ctx, e.Image)
	}
	if !h.Needs(cover != nil) {
		return errTagged
	}
	return tag.Write(src, dst, tag.Info{Title: e.Title, Show: e.Show, Author: e.Author, Date: e.Date}, cover)
}

// place moves a whole download into its show's folder: an MP3 tagged on the way, anything else as
// it came. Cancelled meanwhile (Delete's, maybe), it puts nothing there.
func (l *Library) place(ctx context.Context, part string, e EpisodeInfo) error {
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o755); err != nil {
		return fmt.Errorf("can't make the show's folder: %v", err)
	}
	src := part
	if isMP3(e.Path) {
		tmp := tmpOf(e.Path)
		switch err := l.tagTo(ctx, part, tmp, e); {
		case err == nil:
			src = tmp
		case errors.Is(err, tag.ErrNotMP3), errors.Is(err, errTagged):
		case ctx.Err() != nil:
			os.Remove(tmp)
			return ctx.Err()
		default: // no room for the tagged copy, say: it goes in untagged, for Retag
			log.Printf("tag %s: %v", e.Title, err)
			os.Remove(tmp)
		}
	}
	l.fileMu.Lock() // a Delete's cancel is seen here, or its remove comes after
	if err := ctx.Err(); err != nil {
		l.fileMu.Unlock()
		os.Remove(tmpOf(e.Path))
		return err
	}
	err := os.Rename(src, e.Path)
	l.fileMu.Unlock()
	if err != nil {
		os.Remove(tmpOf(e.Path))
		return fmt.Errorf("can't move the episode into place: %v", err)
	}
	removePart(part)
	if !e.Date.IsZero() {
		os.Chtimes(e.Path, e.Date, e.Date) // newest first where the Q2 sorts by date
	}
	return nil
}

// Retag tags the MP3 episodes on the card that lack a title, an artist, an album or (when the feed
// has one) a cover, one at a time until ctx ends; how many it tagged.
func (l *Library) Retag(ctx context.Context) int {
	l.retagging.Store(true)
	defer l.retagging.Store(false)
	eps, _ := l.Downloaded()
	n := 0
	for _, e := range eps {
		if ctx.Err() != nil {
			break
		}
		if !isMP3(e.Path) {
			continue
		}
		if free, ok := freeBytes(filepath.Dir(e.Path)); ok && free < e.Size+reserve { // no room for its copy
			continue
		}
		tmp := tmpOf(e.Path)
		if err := l.tagTo(ctx, e.Path, tmp, e); err != nil {
			if !errors.Is(err, errTagged) && !errors.Is(err, tag.ErrNotMP3) && ctx.Err() == nil {
				log.Printf("tag %s: %v", e.Title, err)
			}
			continue
		}
		l.fileMu.Lock()
		_, err := os.Stat(e.Path) // deleted meanwhile: not brought back
		if err == nil {
			err = os.Rename(tmp, e.Path)
		}
		l.fileMu.Unlock()
		if err != nil {
			os.Remove(tmp)
			continue
		}
		if !e.Date.IsZero() {
			os.Chtimes(e.Path, e.Date, e.Date)
		}
		n++
	}
	if n > 0 {
		log.Printf("tagged %d episodes", n)
	}
	return n
}

// Retagging is whether Retag is at work.
func (l *Library) Retagging() bool { return l.retagging.Load() }
