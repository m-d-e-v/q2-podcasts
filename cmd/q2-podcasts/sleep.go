package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"q2-podcasts/internal/q2"
	"q2-podcasts/internal/q2spot"
)

// The Sleep Timer pauses playback after a while, or at the end of the episode playing. Q2 Pod keeps
// the timer (addon.c sleep_read): this app writes /tmp/q2-podcasts.sleep, "until <unix time>" or
// "end", and Q2 Pod pauses whatever plays when it is due, then removes the file, so a file still
// there is a timer still running. Removing it cancels.

var sleepChoices = []struct {
	arg, title string
	min        int
}{{"off", "Off", 0}, {"15", "15 Minutes", 15}, {"30", "30 Minutes", 30}, {"45", "45 Minutes", 45},
	{"60", "1 Hour", 60}, {"end", "End of Episode", 0}}

// sleepNow is the timer as the file says: "" off, "end", or the time it pauses.
func (a *app) sleepNow() (string, time.Time) {
	b, err := os.ReadFile(a.opts.sleep)
	if err != nil {
		return "", time.Time{}
	}
	s := strings.TrimSpace(string(b))
	if s == "end" {
		return "end", time.Time{}
	}
	if at, err := strconv.ParseInt(strings.TrimPrefix(s, "until "), 10, 64); err == nil && time.Unix(at, 0).After(time.Now()) {
		return "until", time.Unix(at, 0)
	}
	return "", time.Time{}
}

// sleepLine is the menu row's word on the timer.
func (a *app) sleepLine() (string, bool) {
	switch kind, at := a.sleepNow(); kind {
	case "end":
		return "Pauses at the end of this episode", true
	case "until":
		return "Pauses in " + left(at), true
	}
	return "Off", false
}

// left is the time until at in whole minutes, rounded up (a minute shown while seconds remain).
func left(at time.Time) string {
	sec := int(time.Until(at).Seconds())
	return length((sec + 59) / 60 * 60)
}

// sleepPage is the timer's choices; Selected marks the one running.
func (a *app) sleepPage() q2spot.Reply {
	kind, at := a.sleepNow()
	a.mu.Lock()
	set := a.sleepSet
	a.mu.Unlock()
	var rows []q2spot.Row
	for _, c := range sleepChoices {
		sub := ""
		switch {
		case c.arg == "off" && kind == "":
			sub = "Selected"
		case c.arg == "end" && kind == "end":
			sub = "Selected"
		case c.arg == "end":
			sub = "Pauses when this episode ends"
		case kind == "until" && c.min == set:
			sub = "Selected · " + left(at) + " left"
		}
		rows = append(rows, act("setsleep", c.arg, c.title, sub))
	}
	return q2spot.Reply{Rows: rows}
}

// setSleep starts, replaces or cancels the timer.
func (a *app) setSleep(arg string) q2spot.Reply {
	if arg == "off" {
		os.Remove(a.opts.sleep)
		return q2spot.Reply{Message: "Sleep Timer off"}
	}
	body, msg := "end\n", "Pausing at the end of this episode"
	if arg != "end" {
		m, err := strconv.Atoi(arg)
		if err != nil || m < 1 || m > 600 {
			return q2spot.Fail(q2spot.Bad, "No such time")
		}
		body = fmt.Sprintf("until %d\n", time.Now().Add(time.Duration(m)*time.Minute).Unix())
		msg = "Pausing in " + length(m*60)
		a.mu.Lock()
		a.sleepSet = m
		a.mu.Unlock()
	}
	if err := q2.WriteFile(a.opts.sleep, []byte(body)); err != nil {
		return q2spot.Fail(q2spot.Error, "Couldn't set the Sleep Timer")
	}
	return q2spot.Reply{Message: msg}
}
