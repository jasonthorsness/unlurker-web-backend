package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jasonthorsness/unlurker/hn"
	"github.com/jasonthorsness/unlurker/hn/core"
	"github.com/jasonthorsness/unlurker/unl"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sync/singleflight"
)

func main() {
	ctx := context.Background()

	client, gerr := hn.NewClient(ctx, hn.WithFileCachePath(filepath.Join(os.TempDir(), "hn.db")))
	if gerr != nil {
		log.Fatal(gerr)
	}

	defer func() {
		gerr = client.Close()
		if gerr != nil {
			log.Fatalf("error closing client: %v", gerr)
		}
	}()

	r := gin.Default()

	textCache := core.NewMapCache[*hn.Item, string](core.NewClock(), hn.DefaultCacheFor)

	r.GET("/active", func(c *gin.Context) { handleActive(c, client, textCache) })
	r.GET("/item/:id/tree", func(c *gin.Context) { handleItemDescendants(c, client, textCache) })

	gerr = r.Run()
	if gerr != nil {
		log.Printf("failed to start server: %v", gerr)
	}
}

type handleActiveRoot struct {
	Item *hn.Item
	Time int64
}

type handleActiveResponse struct {
	By     string `json:"by,omitempty"`
	Text   string `json:"text,omitempty"`
	Age    string `json:"age"`
	ID     int    `json:"id"`
	Depth  int    `json:"depth"`
	Active bool   `json:"active,omitempty"`
}

//nolint:cyclop // need parsing helper
func handleActive(c *gin.Context, client *hn.Client, textCache *core.MapCache[*hn.Item, string]) {
	ctx := c.Request.Context()

	window, err := time.ParseDuration(c.DefaultQuery("window", "1h"))
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid window duration"})
		return
	}

	maxAge, err := time.ParseDuration(c.DefaultQuery("max-age", "24h"))
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid max_age duration"})
		return
	}

	minBy, err := strconv.Atoi(c.DefaultQuery("min-by", "3"))
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid min_by"})
		return
	}

	user, err := strconv.Atoi(c.DefaultQuery("user", "1"))
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid user"})
		return
	}

	now := time.Now()
	activeAfter := now.Add(-window)

	roots, tree, err := getActiveRoots(ctx, client, now, activeAfter, maxAge, minBy)
	if err != nil {
		c.PureJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	response := make([]handleActiveResponse, 0, len(roots))

	for _, root := range roots {
		flat := unl.FlattenTree(root.Item, tree)
		activeMap := unl.BuildActiveMap(flat, activeAfter)
		activeMap[root.Item.ID] = unl.ActiveMapChild

		for _, item := range flat {
			t := item.Time
			ae := activeMap[item.ID]
			text := ""

			if item.ID == root.Item.ID {
				t = root.Time
			}

			if ae != 0 {
				text = formatText(item.Item, textCache)
			}

			by := item.By
			if user != 1 {
				by = ""
			}

			response = append(response, handleActiveResponse{
				By:     by,
				Text:   text,
				Age:    unl.PrettyFormatDuration(now.Sub(time.Unix(t, 0))),
				Active: (ae & unl.ActiveMapSelf) > 0,
				ID:     item.ID,
				Depth:  item.Depth,
			})
		}
	}

	c.PureJSON(http.StatusOK, response)
}

func getActiveRoots(
	ctx context.Context,
	client *hn.Client,
	now time.Time,
	activeAfter time.Time,
	maxAge time.Duration,
	minBy int,
) ([]handleActiveRoot, map[int]hn.ItemSet, error) {
	// need to go back quite far in case something has been pulled from the second-chance pool
	const oneWeek = 7 * 24 * time.Hour
	agedAfter := now.Add(-oneWeek)

	items, tree, err := unl.GetActive(ctx, client, activeAfter, agedAfter, minBy, 0)
	if err != nil {
		return nil, nil, err
	}

	frontPageTimes, err := fetchFrontPageTimes(ctx, now)
	if err != nil {
		// ignoring failure to parse updated times for second-chance pool for now
		frontPageTimes = nil
	}

	roots := make([]handleActiveRoot, 0, len(items))

	agedAfter = time.Now().Add(-maxAge)

	for _, item := range items {
		t := item.Time

		updated, ok := frontPageTimes[item.ID]
		if ok && updated > item.Time {
			t = updated
		}

		if time.Unix(t, 0).After(agedAfter) {
			roots = append(roots, handleActiveRoot{item, t})
		}
	}

	sort.Slice(roots, func(i, j int) bool {
		a, b := roots[i], roots[j]
		if a.Time == b.Time {
			return a.Item.ID > b.Item.ID
		}

		return a.Time > b.Time
	})

	return roots, tree, nil
}

type handleItemDescendantsResponse struct {
	By    string `json:"by,omitempty"`
	Text  string `json:"text,omitempty"`
	Time  int64  `json:"time"`
	ID    int    `json:"id"`
	Depth int    `json:"depth"`
}

func handleItemDescendants(c *gin.Context, client *hn.Client, textCache *core.MapCache[*hn.Item, string]) {
	ctx := c.Request.Context()

	idParam := c.Param("id")

	itemID, err := strconv.Atoi(idParam)
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	items, err := client.GetItems(ctx, []int{itemID})
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "failed to retrieve item"})
		return
	}

	item := items[itemID]

	all, err := client.GetDescendants(ctx, items)
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "failed to retrieve item descendants"})
		return
	}

	allByParent, _, err := all.GroupByParent()
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "failed to group item descendants by parent"})
		return
	}

	flat := unl.FlattenTree(item, allByParent)

	response := make([]handleItemDescendantsResponse, 0, len(flat))

	user, err := strconv.Atoi(c.DefaultQuery("user", "1"))
	if err != nil {
		c.PureJSON(http.StatusBadRequest, gin.H{"error": "invalid user"})
		return
	}

	for _, f := range flat {
		by := f.By
		if user != 1 {
			by = ""
		}

		response = append(response, handleItemDescendantsResponse{
			By:    by,
			Text:  formatText(f.Item, textCache),
			Time:  f.Time,
			ID:    f.ID,
			Depth: f.Depth,
		})
	}

	c.PureJSON(http.StatusOK, response)
}

func formatText(item *hn.Item, textCache *core.MapCache[*hn.Item, string]) string {
	found, _ := textCache.Get([]*hn.Item{item})
	if len(found) > 0 {
		return found[0].Value
	}

	text := unl.PrettyFormatTitle(item, true)
	textCache.Put(item, text)

	return text
}

var fetchGroup singleflight.Group //nolint:gochecknoglobals

var fetchCache atomic.Value //nolint:gochecknoglobals

var frontPageAgeExtractor = regexp.MustCompile(
	`<span class="age" title="[^"]+\s+(\d+)"><a href="item\?id=(\d+)">([^<]+) ago</a></span>`)

type fetchCacheEntry struct {
	data map[int]int64
	ts   time.Time
}

var errStatusNotOK = errors.New("status not ok")

func fetchFrontPageTimes(ctx context.Context, now time.Time) (map[int]int64, error) {
	entry, ok := fetchCache.Load().(*fetchCacheEntry)
	if ok {
		if time.Since(entry.ts) < time.Minute {
			return entry.data, nil
		}
	}

	v, err, _ := fetchGroup.Do(
		"frontpage",
		func() (interface{}, error) { return fetchFrontPageTimesInner(ctx, now) })
	if err != nil {
		return nil, fmt.Errorf("singleflight frontpage failed: %w", err)
	}

	times := v.(map[int]int64) //nolint:forcetypeassert // typed return

	return times, nil
}

func fetchFrontPageTimesInner(ctx context.Context, now time.Time) (interface{}, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://news.ycombinator.com", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	res, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errStatusNotOK, res.Status)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	matches := frontPageAgeExtractor.FindAllSubmatch(body, -1)
	m := make(map[int]int64, len(matches))

	for _, match := range matches {
		ts, err := strconv.ParseInt(string(match[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse time: %w", err)
		}

		t := time.Unix(ts, 0)

		id, err := strconv.Atoi(string(match[2]))
		if err != nil {
			return nil, fmt.Errorf("failed to parse id: %w", err)
		}

		age, err := parseAge(string(match[3]))
		if err != nil {
			return nil, err
		}

		diff := now.Sub(t) - age
		if diff > 2*time.Hour {
			m[id] = now.Add(-age).Unix()
		} else {
			m[id] = ts
		}
	}

	fetchCache.Store(&fetchCacheEntry{
		data: m,
		ts:   time.Now(),
	})

	return m, nil
}

var errUnexpectedAgeFormat = errors.New("unexpected age format")

var relativeAgeRegex = regexp.MustCompile(
	`^\s*(\d+)\s+(hour|hours|minute|minutes|day|days)\s*$`)

func parseAge(s string) (time.Duration, error) {
	m := relativeAgeRegex.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%w: %q", errUnexpectedAgeFormat, s)
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("failed to parse age: %w", err)
	}

	switch m[2] {
	case "minute", "minutes":
		return time.Duration(n) * time.Minute, nil
	case "hour", "hours":
		return time.Duration(n) * time.Hour, nil
	case "day", "days":
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("%w: %q", errUnexpectedAgeFormat, m[2])
	}
}
