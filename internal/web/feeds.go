// SPDX-FileCopyrightText: 2026 The Rabbit Hole Authors
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/DanielBlei/rabbithole/internal/cluster"
	"github.com/DanielBlei/rabbithole/internal/config"
	"github.com/DanielBlei/rabbithole/internal/store"
)

// feedStripLen is how many past fetch attempts the per-feed history strip
// shows. Enough to see a pattern ("failing since three runs ago") without
// turning the row into a chart.
const feedStripLen = 12

// Feed row display states.
const (
	feedStateOK    = "ok"
	feedStateError = "error"
	feedStateNever = "never" // configured but never fetched
	feedStateOff   = "off"   // parked
)

// feedRowData is one feed's line on the Sources page. The *On strings name
// where an inherited value came from (rendered as the asterisk's tooltip); a
// value the feed sets itself has an empty *On.
type feedRowData struct {
	ID   string
	Name string
	URL  string
	Host string // URL's host — the compact label under the feed name
	Type string // resolved kind; the row badges anything but rss

	Enabled bool
	Deleted bool
	Since   string
	SinceOn string
	Cap     string // max_items, or "—" when uncapped
	CapOn   string
	Tags    []string
	// TagsAttr is Tags comma-joined — the row's data-tags, which the page's
	// client-side search matches against alongside the name and URL.
	TagsAttr string

	// Selected marks the feed whose settings the pane beside the list is
	// showing, so the row you are editing is visible as the one you are editing.
	Selected bool

	// Health, aggregated from the feed's fetch history.
	State      string
	Detail     string // error message, or the item-count phrase
	LastFetch  string // relative time of the latest attempt, "" when never
	FailStreak int    // consecutive failures; >1 is worth showing
	Since1st   string // when the current failure run began
	Strip      []feedTick
}

// feedTick is one attempt in the history strip, oldest-first for display.
type feedTick struct {
	OK    bool
	Title string // hover text: outcome + when
}

// toFeedRow shapes one resolved feed plus its health into a display row.
func toFeedRow(f config.ResolvedFeed, h store.FeedHealth, now time.Time) feedRowData {
	row := feedRowData{
		ID:       f.ID,
		Name:     f.Name,
		URL:      f.URL,
		Host:     hostOf(f.URL),
		Type:     string(f.Type),
		Enabled:  f.Enabled,
		Since:    shortDur(f.Since),
		Cap:      capLabel(f.MaxItems),
		Tags:     f.Tags,
		TagsAttr: strings.Join(f.Tags, ","),
	}
	if f.SinceFrom.Inherited() {
		row.SinceOn = string(f.SinceFrom)
	}
	if f.MaxItemsFrom.Inherited() {
		row.CapOn = string(f.MaxItemsFrom)
	}

	// A disabled feed reports as parked regardless of what its last fetch did:
	// stale history from before it was switched off would read as a live
	// problem. Its strip is still worth showing — that's the point of keeping
	// history — so it's built either way.
	row.Strip = toFeedStrip(h.Recent, now)
	if !f.Enabled {
		row.State, row.Detail = feedStateOff, "disabled"
		return row
	}

	switch {
	case h.LastFetch.IsZero():
		row.State, row.Detail = feedStateNever, "not fetched yet"
	case h.OK():
		row.State = feedStateOK
		row.Detail = itemsPhrase(h.Items)
		row.LastFetch = agoPhrase(h.LastFetch, now)
	default:
		row.State = feedStateError
		row.Detail = h.Error
		row.LastFetch = agoPhrase(h.LastFetch, now)
		row.FailStreak = h.FailStreak
		if since, failing := h.FailingSince(); failing {
			row.Since1st = agoPhrase(since, now)
		}
	}
	return row
}

// toFeedStrip renders the attempt history oldest-first, so the strip reads
// left-to-right as time moving forward and the newest tick sits nearest the
// status text it explains.
func toFeedStrip(attempts []store.FeedAttempt, now time.Time) []feedTick {
	if len(attempts) == 0 {
		return nil
	}
	out := make([]feedTick, 0, len(attempts))
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		title := itemsPhrase(a.Items)
		if !a.OK() {
			title = a.Error
			if title == "" {
				title = "failed"
			}
		}
		out = append(out, feedTick{OK: a.OK(), Title: title + " · " + agoPhrase(a.At, now)})
	}
	return out
}

// itemsPhrase renders a fetch's item count with correct pluralization.
func itemsPhrase(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}

// defaultsSummary renders the set-wide defaults as one line for the page's
// legend, naming the built-in value for anything they leave unset, so the
// cascade is legible without opening the defaults editor.
func defaultsSummary(d config.FeedDefaults) string {
	var parts []string
	if d.Since != nil {
		parts = append(parts, "since "+shortDur(d.Since.Std()))
	} else {
		parts = append(parts, "since "+shortDur(config.DefaultSince)+" (built-in)")
	}
	if d.MaxItems != nil {
		parts = append(parts, "max_items "+capLabel(*d.MaxItems))
	} else {
		parts = append(parts, "uncapped")
	}
	if d.Enabled != nil && !*d.Enabled {
		parts = append(parts, "disabled by default")
	}
	return strings.Join(parts, " · ")
}

// capLabel renders a max_items value; zero means uncapped.
func capLabel(n int) string {
	if n <= 0 {
		return "—"
	}
	return strconv.Itoa(n)
}

// shortDur renders a lookback window the way it's written in config — "7d",
// "36h", "90m" — rather than Go's "168h0m0s".
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return d.String()
	}
}

// hostOf pulls the host out of a feed URL for the compact cell label, falling
// back to the raw string when it isn't a parseable absolute URL.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

type feedGroup struct {
	Representative rowData
	Members        []rowData
	MemberCount    int
}

func rowIDs(rows []rowData) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	return ids
}

func toFeedGroups(rows []rowData, clusters []store.StoryCluster) []feedGroup {
	clusterOf := make(map[string]int, len(rows))
	for i, cluster := range clusters {
		for _, item := range cluster.Items {
			clusterOf[item.ItemID] = i
		}
	}
	seen := make(map[int]struct{}, len(clusters))
	groups := make([]feedGroup, 0, len(rows))
	for _, row := range rows {
		idx, ok := clusterOf[row.ID]
		if !ok {
			groups = append(groups, feedGroup{Representative: row})
			continue
		}
		if _, done := seen[idx]; done {
			continue
		}
		seen[idx] = struct{}{}
		group := feedGroup{Representative: row}
		for _, other := range rows {
			if other.ID == row.ID {
				continue
			}
			if member, ok := clusterOf[other.ID]; ok && member == idx {
				group.Members = append(group.Members, other)
			}
		}
		group.MemberCount = len(group.Members)
		groups = append(groups, group)
	}
	return groups
}

func feedGroupToggleURL(q url.Values, grouped bool) string {
	v := make(url.Values, len(q))
	for key, values := range q {
		v[key] = append([]string(nil), values...)
	}
	if grouped {
		v.Del("grouped")
	} else {
		v.Set("grouped", "1")
	}
	if encoded := v.Encode(); encoded != "" {
		return "/feed?" + encoded
	}
	return "/feed"
}

func exactStoryClusters(rows []rowData) []store.StoryCluster {
	items := make([]cluster.Item, len(rows))
	for i, row := range rows {
		items[i] = cluster.Item{ID: row.ID, Source: row.Source, Title: row.Title, Link: row.Link}
	}
	found, _, err := cluster.Build(context.Background(), items, cluster.Options{MaxItems: len(items)})
	if err != nil {
		return nil
	}
	out := make([]store.StoryCluster, len(found))
	for i, c := range found {
		members := make([]store.StoryClusterItem, len(c.Items))
		for j, item := range c.Items {
			members[j] = store.StoryClusterItem{ItemID: item.ItemID, Position: item.Position}
		}
		out[i] = store.StoryCluster{
			ID: c.ID, RepresentativeItemID: c.RepresentativeItemID,
			Method: c.Method, Confidence: c.Confidence, Items: members,
		}
	}
	return out
}
