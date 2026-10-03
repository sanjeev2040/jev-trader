package news

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	maxFeedBytes = 8 << 20
	maxSummary   = 600
	// userAgent identifies us to the feed hosts; unlike NSE they accept it.
	userAgent = "Mozilla/5.0 (compatible; jevtrader/1.0; +https://github.com/sanjeev2040/jev-trader)"
)

// RSS is a source reading one RSS 2.0 feed. Feeds carry only the newest items
// and no date filter, so a window in the past finds nothing.
type RSS struct {
	// FeedName names the source, for example "economictimes-stocks".
	FeedName string
	URL      string
	Client   *http.Client
}

func (r RSS) Name() string { return r.FeedName }

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Items   []struct {
		Title       string `xml:"title"`
		Description string `xml:"description"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		PubDate     string `xml:"pubDate"`
	} `xml:"channel>item"`
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// Fetch returns the feed's items published in [from, to).
func (r RSS) Fetch(ctx context.Context, from, to time.Time) ([]Item, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.FeedName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", r.FeedName, resp.StatusCode)
	}
	return parseRSS(r.FeedName, io.LimitReader(resp.Body, maxFeedBytes), from, to)
}

func parseRSS(name string, body io.Reader, from, to time.Time) ([]Item, error) {
	var feed rssFeed
	if err := xml.NewDecoder(body).Decode(&feed); err != nil {
		return nil, fmt.Errorf("%s: not an RSS feed: %w", name, err)
	}
	var out []Item
	for _, it := range feed.Items {
		pub, ok := parseTime(it.PubDate)
		if !ok || pub.Before(from) || !pub.Before(to) {
			continue
		}
		title := clean(it.Title)
		if title == "" {
			continue
		}
		id := strings.TrimSpace(it.GUID)
		if id == "" {
			id = strings.TrimSpace(it.Link)
		}
		out = append(out, Item{
			Source: name, ID: id, Title: title, Summary: truncate(clean(it.Description), maxSummary),
			URL: strings.TrimSpace(it.Link), Published: pub,
		})
	}
	return out, nil
}

var timeLayouts = []string{time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", time.RFC3339}

func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// clean strips markup and entities from a feed field.
func clean(s string) string {
	s = tagRE.ReplaceAllString(html.UnescapeString(s), " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	// Do not end in the middle of a UTF-8 sequence.
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}
