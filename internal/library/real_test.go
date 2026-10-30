//go:build realfeeds

// The library against real podcasts on the internet, from the directory's search to each newest
// episode's first KB through the hosts' tracking redirects:
//
//	go test -tags realfeeds -run TestRealFeeds -v ./internal/library
//
// Not part of the normal suite: it needs the network.
package library

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"q2-podcasts/internal/directory"
	"q2-podcasts/internal/tlsroots"
)

var shows = []string{"The Daily", "99% Invisible", "In Our Time", "Radiolab", "Hardcore History",
	"Freakonomics Radio", "The Rest Is History", "Darknet Diaries", "Accidental Tech Podcast", "Song Exploder",
	"Serial", "Huberman Lab", "Desert Island Discs", "Reply All", "Off Menu"}

func TestRealFeeds(t *testing.T) {
	tr := tlsroots.Transport()
	client := &http.Client{Transport: tr, Timeout: 60 * time.Second}
	dir := t.TempDir()
	l, err := Open(Options{Dir: filepath.Join(dir, ".podcasts"), Root: filepath.Join(dir, "Podcasts"),
		Client: client, Fetch: &http.Client{Transport: tr}, Agent: "Q2Podcasts/1.0 (Shanling Q2; Q2 Pod card app)",
		Ready: func() bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shows {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		found, err := directory.Search(ctx, client, name, 1)
		if err != nil || len(found) == 0 {
			t.Errorf("%s: search %v", name, err)
			cancel()
			continue
		}
		start := time.Now()
		si, err := l.Subscribe(ctx, found[0].FeedURL)
		if err != nil {
			t.Errorf("%s (%s): %v", name, found[0].FeedURL, err)
			cancel()
			continue
		}
		eps, _ := l.Episodes(si.Key)
		e := eps[0]
		// The newest episode's first KB, through the trackers' redirects, over verified TLS.
		u, _ := l.source(si.Key, e.EID)
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Range", "bytes=0-1023")
		req.Header.Set("User-Agent", "Q2Podcasts/1.0 (Shanling Q2; Q2 Pod card app)")
		status := "?"
		if resp, err := (&http.Client{Transport: tr, Timeout: 60 * time.Second}).Do(req); err != nil {
			status = "ERR " + err.Error()
			t.Errorf("%s: enclosure %v", name, err)
		} else {
			status = fmt.Sprintf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Request.URL.Host)
			resp.Body.Close()
			if resp.StatusCode != 206 && resp.StatusCode != 200 {
				t.Errorf("%s: enclosure HTTP %d", name, resp.StatusCode)
			}
		}
		fmt.Printf("%-24s %4d eps %5.1fs | %s | %s | %ds | %s\n   -> %s\n   enclosure: %s\n",
			si.Title, si.Episodes, time.Since(start).Seconds(), e.Date.Format("2006-01-02"), e.Title, e.Duration,
			si.Folder, filepath.Base(e.Path), status)
		cancel()
	}
}
