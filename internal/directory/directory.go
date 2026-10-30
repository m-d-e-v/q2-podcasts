// Package directory finds podcasts' feeds through Apple's public iTunes Search API, which needs no
// key: a search by name, and the feed behind an Apple Podcasts link (podcasts.apple.com/…/id123).
package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Base is the API's address; tests point it elsewhere.
var Base = "https://itunes.apple.com"

// Country is the store searched; results are much the same in every one.
var Country = "US"

type Show struct {
	Title    string `json:"title"`
	Author   string `json:"author"`
	FeedURL  string `json:"url"`
	Genre    string `json:"genre,omitempty"`
	Episodes int    `json:"episodes,omitempty"`
}

var ErrUnavailable = errors.New("the podcast directory can't be reached")

type result struct {
	CollectionName   string `json:"collectionName"`
	ArtistName       string `json:"artistName"`
	FeedURL          string `json:"feedUrl"`
	TrackCount       int    `json:"trackCount"`
	PrimaryGenreName string `json:"primaryGenreName"`
}

func get(ctx context.Context, c *http.Client, path string, q url.Values) ([]Show, error) {
	q.Set("country", Country)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrUnavailable, resp.StatusCode)
	}
	var body struct {
		Results []result `json:"results"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 2<<20)).Decode(&body); err != nil {
		return nil, ErrUnavailable
	}
	var out []Show
	for _, r := range body.Results {
		if r.FeedURL == "" { // Apple-only shows have no public feed
			continue
		}
		out = append(out, Show{Title: strings.TrimSpace(r.CollectionName), Author: strings.TrimSpace(r.ArtistName),
			FeedURL: r.FeedURL, Genre: r.PrimaryGenreName, Episodes: r.TrackCount})
	}
	return out, nil
}

// Search finds up to limit podcasts by name.
func Search(ctx context.Context, c *http.Client, term string, limit int) ([]Show, error) {
	return get(ctx, c, "/search", url.Values{"media": {"podcast"}, "entity": {"podcast"},
		"term": {term}, "limit": {fmt.Sprint(limit)}})
}

var appleID = regexp.MustCompile(`^https?://(?:podcasts|itunes)\.apple\.com/.*?/id(\d+)`)

// AppleID is the show id in an Apple Podcasts link, "" when s isn't one.
func AppleID(s string) string {
	if m := appleID.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1]
	}
	return ""
}

// Lookup is the feed of the show with this Apple id.
func Lookup(ctx context.Context, c *http.Client, id string) (Show, error) {
	shows, err := get(ctx, c, "/lookup", url.Values{"id": {id}, "entity": {"podcast"}})
	if err != nil {
		return Show{}, err
	}
	if len(shows) == 0 {
		return Show{}, errors.New("Apple Podcasts has no public feed for that show")
	}
	return shows[0], nil
}
