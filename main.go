package main

import (
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jasonthorsness/unlurker/hn"
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

	r.GET("/active", func(c *gin.Context) {
		ctx := c.Request.Context()

		window, err := time.ParseDuration(c.DefaultQuery("window", "1h"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window duration"})
			return
		}

		maxAge, err := time.ParseDuration(c.DefaultQuery("max-age", "24h"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid max_age duration"})
			return
		}

		minBy, err := strconv.Atoi(c.DefaultQuery("min-by", "3"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid min_by"})
			return
		}

		now := time.Now()
		activeAfter := now.Add(-window)
		agedAfter := now.Add(-maxAge)

		items, tree, err := unl.GetActive(ctx, client, activeAfter, agedAfter, minBy, 0)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		pw := &prettyWriter{now: now, activeAfter: activeAfter, lines: []prettyLine{}}
		for _, item := range items {
			pw.writeTree(item, tree)
		}

		c.JSON(http.StatusOK, pw.lines)
	})

	gerr = r.Run()
	if gerr != nil {
		log.Printf("failed to start server: %v", gerr)
	}
}

type prettyLine struct {
	By     string `json:"by"`
	Age    string `json:"age"`
	Indent string `json:"indent"`
	Text   string `json:"text"`
	ID     int    `json:"id"`
	Root   bool   `json:"root"`
	Active bool   `json:"active"`
}

type prettyWriter struct {
	now         time.Time
	activeAfter time.Time
	lines       []prettyLine
}

func (pw *prettyWriter) writeTree(item *hn.Item, allByParent map[int]hn.ItemSet) {
	pw.writeTreeRecurse(item, allByParent, "")
}

func (pw *prettyWriter) writeTreeRecurse(item *hn.Item, allByParent map[int]hn.ItemSet, indent string) {
	isActive := time.Unix(item.Time, 0).After(pw.activeAfter) && !item.Dead && !item.Deleted
	hasActiveChild := findActiveChild(item, allByParent, pw.activeAfter)

	pw.writeItemIndent(item, isActive || hasActiveChild || item.Parent == nil, isActive, indent)

	children := allByParent[item.ID]
	cc := children.Slice()

	for i, child := range cc {
		var childIndent string

		if i != len(cc)-1 {
			childIndent = indent + "|"
		} else {
			childIndent = indent + " "
		}

		pw.writeTreeRecurse(child, allByParent, childIndent)
	}
}

func findActiveChild(item *hn.Item, allByParent map[int]hn.ItemSet, activeAfter time.Time) bool {
	for _, child := range allByParent[item.ID] {
		if time.Unix(child.Time, 0).After(activeAfter) && !child.Dead && !child.Deleted {
			return true
		}
	}

	return false
}

func (pw *prettyWriter) writeItemIndent(item *hn.Item, showText bool, isActive bool, indent string) {
	by := item.By
	age := prettyFormatDuration(pw.now.Sub(time.Unix(item.Time, 0)))
	text := ""

	if showText {
		text = unl.PrettyFormatTitle(item, true)
	}

	text = html.UnescapeString(text)

	pw.lines = append(pw.lines, prettyLine{by, age, indent, text, item.ID, item.Parent == nil, isActive})
}

func prettyFormatDuration(d time.Duration) string {
	totalMinutes := int(d.Minutes())

	const minutesPerHour = 60

	if totalMinutes < minutesPerHour {
		return fmt.Sprintf("%dm", totalMinutes)
	}

	hours := totalMinutes / minutesPerHour
	minutes := totalMinutes % minutesPerHour

	return fmt.Sprintf("%dh %2dm", hours, minutes)
}
