package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jasonthorsness/unlurker/hn"
	"github.com/jasonthorsness/unlurker/hn/core"
	"github.com/jasonthorsness/unlurker/unl"
	_ "github.com/mattn/go-sqlite3"
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

type handleActiveResponseItem struct {
	By           string `json:"by,omitempty"`
	Text         string `json:"text,omitempty"`
	Age          string `json:"age"`
	ID           int    `json:"id"`
	Depth        int    `json:"depth"`
	Active       bool   `json:"active,omitempty"`
	SecondChance bool   `json:"secondchance,omitempty"`
}

type handleActiveResponse struct {
	Items              []handleActiveResponseItem `json:"items"`
	SecondChanceFailed bool                       `json:"secondChanceFailed"`
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

	roots, tree, secondChanceFailed, err := getActiveRoots(ctx, client, now, activeAfter, maxAge, minBy)
	if err != nil {
		c.PureJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	const estimatedItemsPerRoot = 10
	items := make([]handleActiveResponseItem, 0, len(roots)*estimatedItemsPerRoot)

	for _, root := range roots {
		flat := unl.FlattenTree(root.Item, tree)
		activeMap := unl.BuildActiveMap(flat, activeAfter)
		activeMap[root.Item.ID] = unl.ActiveMapChild

		for _, item := range flat {
			t := item.Time
			ae := activeMap[item.ID]
			text := ""

			secondChance := false

			if item.ID == root.Item.ID {
				t = root.Time
				secondChance = item.Time != root.Time
			}

			if ae != 0 {
				text = formatText(item.Item, textCache)
			}

			by := item.By
			if user != 1 {
				by = ""
			}

			items = append(items, handleActiveResponseItem{
				By:           by,
				Text:         text,
				Age:          unl.PrettyFormatDuration(now.Sub(time.Unix(t, 0))),
				Active:       (ae & unl.ActiveMapSelf) > 0,
				ID:           item.ID,
				Depth:        item.Depth,
				SecondChance: secondChance,
			})
		}
	}

	response := handleActiveResponse{
		Items:              items,
		SecondChanceFailed: secondChanceFailed,
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
) ([]handleActiveRoot, map[int]hn.ItemSet, bool, error) {
	var secondChanceFailed bool

	frontPageTimes, err := unl.FetchFrontPageTimes(ctx, now)
	if err != nil {
		frontPageTimes = nil
		secondChanceFailed = true
	}

	agedAfter := time.Now().Add(-maxAge)

	items, tree, err := unl.GetActive(ctx, client, frontPageTimes, activeAfter, agedAfter, minBy, 0)
	if err != nil {
		return nil, nil, secondChanceFailed, err
	}

	roots := make([]handleActiveRoot, 0, len(items))

	for _, item := range items {
		t := item.Time

		adjusted, ok := frontPageTimes[item.ID]
		if ok {
			t = adjusted
		}

		if time.Unix(t, 0).After(agedAfter) {
			roots = append(roots, handleActiveRoot{item, t})
		}
	}

	return roots, tree, secondChanceFailed, nil
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
