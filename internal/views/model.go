package views

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bograh/telegram-cleanup/internal/store"
)

// Job is a snapshot of the background scan/analysis job.
type Job struct {
	Name    string
	Running bool
	Done    int
	Total   int
	Err     string
	Msg     string
	Result  string // shown as a toast when the job finishes
	// Stopping is set once the user clicked Stop and the job is winding down.
	Stopping bool
}

func (j Job) Percent() int {
	if j.Total == 0 {
		return 0
	}
	return int(math.Round(float64(j.Done) * 100 / float64(j.Total)))
}

// Filter is the dashboard query state.
type Filter struct {
	Q    string
	Kind string
	Cat  string
	Rec  string
	Sort string
}

// Count is a label with a number, for chips.
type Count struct {
	Label string
	N     int
}

// Dashboard is everything the main page renders.
type Dashboard struct {
	Me           string
	AIEnabled    bool
	AIModel      string
	Job          Job
	Overview     *store.Overview
	ScannedAt    time.Time
	Total        int
	Channels     int
	Groups       int
	Bots         int
	Deleted      int
	Unread       int
	Analysed     int
	Recs         []Count
	Categories   []Count
	Chats        []store.Chat
	Filter       Filter
	ShownCount   int
	FlashMessage string
}

// Build computes stats and the filtered/sorted chat list.
func Build(all []store.Chat, f Filter) Dashboard {
	d := Dashboard{Filter: f, Total: len(all)}
	recs := map[string]int{}
	cats := map[string]int{}
	for _, c := range all {
		switch c.Kind {
		case store.KindChannel:
			d.Channels++
		case store.KindGroup:
			d.Groups++
		case store.KindBot:
			d.Bots++
		case store.KindDeleted:
			d.Deleted++
		}
		d.Unread += c.Unread
		if c.Analysis != nil {
			d.Analysed++
			recs[c.Analysis.Recommendation]++
			cats[c.Analysis.Category]++
		}
	}
	for _, r := range []string{store.RecLeave, store.RecArchive, store.RecMute, store.RecKeep} {
		d.Recs = append(d.Recs, Count{r, recs[r]})
	}
	for k, v := range cats {
		d.Categories = append(d.Categories, Count{k, v})
	}
	sort.Slice(d.Categories, func(i, j int) bool {
		if d.Categories[i].N != d.Categories[j].N {
			return d.Categories[i].N > d.Categories[j].N
		}
		return d.Categories[i].Label < d.Categories[j].Label
	})

	q := strings.ToLower(strings.TrimSpace(f.Q))
	for _, c := range all {
		if f.Kind != "" && string(c.Kind) != f.Kind {
			continue
		}
		if f.Rec != "" && (c.Analysis == nil || c.Analysis.Recommendation != f.Rec) {
			continue
		}
		if f.Cat != "" && (c.Analysis == nil || c.Analysis.Category != f.Cat) {
			continue
		}
		if q != "" {
			hay := strings.ToLower(c.Title + " " + c.Username)
			if c.Analysis != nil {
				hay += " " + strings.ToLower(c.Analysis.Summary)
			}
			if !strings.Contains(hay, q) {
				continue
			}
		}
		d.Chats = append(d.Chats, c)
	}
	sortChats(d.Chats, f.Sort)
	d.ShownCount = len(d.Chats)
	return d
}

func sortChats(cs []store.Chat, by string) {
	noise := func(c store.Chat) int {
		if c.Analysis == nil {
			return -1
		}
		return c.Analysis.Noise
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch by {
		case "unread":
			return a.Unread > b.Unread
		case "members":
			return a.Members > b.Members
		case "title":
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		case "last":
			return a.LastMessage.Before(b.LastMessage)
		case "noise":
			return noise(a) > noise(b)
		default: // activity
			return a.MsgsPerDay > b.MsgsPerDay
		}
	})
}

func kindLabel(k store.Kind) string {
	switch k {
	case store.KindChannel:
		return "Channel"
	case store.KindGroup:
		return "Group"
	case store.KindBot:
		return "Bot"
	case store.KindDeleted:
		return "Deleted account"
	}
	return string(k)
}

func recLabel(r string) string {
	switch r {
	case store.RecLeave:
		return "Leave"
	case store.RecArchive:
		return "Archive"
	case store.RecMute:
		return "Mute"
	case store.RecKeep:
		return "Keep"
	}
	return r
}

func compact(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func rate(r float64) string {
	switch {
	case r == 0:
		return "—"
	case r < 1:
		return fmt.Sprintf("%.1f/day", r)
	}
	return fmt.Sprintf("%.0f/day", r)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "just now"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
}

func initial(s string) string {
	for _, r := range s {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func avatarHue(key string) string {
	h := 0
	for _, r := range key {
		h = (h*31 + int(r)) % 360
	}
	return fmt.Sprintf("--h:%d", h)
}

func selected(a, b string) bool { return a == b }

func recOf(c store.Chat) string {
	if c.Analysis == nil {
		return ""
	}
	return c.Analysis.Recommendation
}
