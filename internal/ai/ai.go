// Package ai classifies chats using any OpenAI-compatible Chat Completions API
// (OpenAI, OpenRouter, Groq, Together, Ollama, LM Studio, vLLM, ...).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bograh/telegram-cleanup/internal/store"
)

// Categories the model must choose from. Kept small so the dashboard filters stay useful.
var Categories = []string{
	"news", "crypto & trading", "promo & spam", "tech", "work & study",
	"community", "friends & family", "entertainment", "shopping & deals",
	"utility bot", "dead / inactive", "other",
}

type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string
	// JSONMode sends response_format={"type":"json_object"}. Some local servers
	// don't support it; set AI_JSON_MODE=false for those.
	JSONMode  bool
	BatchSize int
	Workers   int
}

type Analyzer struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Analyzer {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 25
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 3
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Analyzer{cfg: cfg, http: &http.Client{Timeout: 3 * time.Minute}}
}

func (a *Analyzer) Model() string { return a.cfg.Model }

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []message `json:"messages"`
	Temperature    float64   `json:"temperature"`
	ResponseFormat any       `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// complete sends one chat completion and decodes the JSON object it returns into out.
func (a *Analyzer) complete(ctx context.Context, system, user string, out any) error {
	req := chatRequest{
		Model:       a.cfg.Model,
		Messages:    []message{{"system", system}, {"user", user}},
		Temperature: 0.2,
	}
	if a.cfg.JSONMode {
		req.ResponseFormat = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(req)

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return err
		}
		hr.Header.Set("Content-Type", "application/json")
		if a.cfg.APIKey != "" {
			hr.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
		}
		resp, err := a.http.Do(hr)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("AI API %s: %s", resp.Status, truncate(string(raw), 300))
			continue
		}
		var cr chatResponse
		if err := json.Unmarshal(raw, &cr); err != nil {
			return fmt.Errorf("AI API %s: %s", resp.Status, truncate(string(raw), 300))
		}
		if cr.Error != nil {
			return fmt.Errorf("AI API error: %s", cr.Error.Message)
		}
		if resp.StatusCode >= 400 || len(cr.Choices) == 0 {
			return fmt.Errorf("AI API %s: %s", resp.Status, truncate(string(raw), 300))
		}
		content := extractJSON(cr.Choices[0].Message.Content)
		if err := json.Unmarshal([]byte(content), out); err != nil {
			lastErr = fmt.Errorf("model returned invalid JSON (finish_reason=%s): %w", cr.Choices[0].FinishReason, err)
			continue
		}
		return nil
	}
	return lastErr
}

// extractJSON tolerates models that wrap JSON in ``` fences or add prose.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const classifySystem = `You help a user declutter their Telegram account. You receive a JSON list of channels, groups and bots the user is in, with stats and a few recent message snippets. For EACH item decide:

- category: exactly one of %s
- summary: one short sentence on what the chat is about (max ~15 words). Write it in English even if the chat isn't.
- recommendation: one of "keep", "mute", "archive", "leave"
    keep    = useful and not noisy, or something the user clearly participates in / owns
    mute    = useful but posts too often
    archive = occasionally useful, rarely needed day to day
    leave   = spam, scams, ads, crypto pump/airdrop shilling, dead or abandoned, or obviously irrelevant
- reason: max ~12 words explaining the recommendation
- noise: integer 0-10 (10 = constant spam/ads, 0 = quiet and valuable)

Signals: msgs_per_day (activity), unread (how much the user ignores it), muted/archived (user already deprioritised it), owner (never recommend leaving chats the user owns), scam_flag (Telegram flagged it), last_message (old = dead).

Return ONLY a JSON object: {"results":[{"key":"...","category":"...","summary":"...","recommendation":"...","reason":"...","noise":0}]} with one entry per input key.`

type chatInput struct {
	Key         string   `json:"key"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	Username    string   `json:"username,omitempty"`
	Members     int      `json:"members,omitempty"`
	MsgsPerDay  float64  `json:"msgs_per_day"`
	Unread      int      `json:"unread"`
	Muted       bool     `json:"muted,omitempty"`
	Archived    bool     `json:"archived,omitempty"`
	Owner       bool     `json:"owner,omitempty"`
	ScamFlag    bool     `json:"scam_flag,omitempty"`
	LastMessage string   `json:"last_message,omitempty"`
	Samples     []string `json:"recent_messages,omitempty"`
}

type classifyOutput struct {
	Results []struct {
		Key string `json:"key"`
		store.Analysis
	} `json:"results"`
}

// Progress is called with (done, total) chats analysed.
type Progress func(done, total int)

// Classify analyses chats in parallel batches. Partial results are returned
// alongside the first error so a flaky batch doesn't throw away the rest.
func (a *Analyzer) Classify(ctx context.Context, chats []store.Chat, progress Progress) (map[string]store.Analysis, error) {
	cats, _ := json.Marshal(Categories)
	system := fmt.Sprintf(classifySystem, string(cats))

	var batches [][]store.Chat
	for i := 0; i < len(chats); i += a.cfg.BatchSize {
		batches = append(batches, chats[i:min(i+a.cfg.BatchSize, len(chats))])
	}

	var (
		mu       sync.Mutex
		results  = map[string]store.Analysis{}
		firstErr error
		done     int
		wg       sync.WaitGroup
		sem      = make(chan struct{}, a.cfg.Workers)
	)
	if progress != nil {
		progress(0, len(chats))
	}
	for _, b := range batches {
		wg.Add(1)
		go func(b []store.Chat) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			in := make([]chatInput, len(b))
			for i, c := range b {
				in[i] = chatInput{
					Key: c.Key, Kind: string(c.Kind), Title: c.Title, Username: c.Username,
					Members: c.Members, MsgsPerDay: c.MsgsPerDay, Unread: c.Unread,
					Muted: c.Muted, Archived: c.Archived, Owner: c.Owner, ScamFlag: c.ScamFlag,
					Samples: c.Samples,
				}
				if !c.LastMessage.IsZero() {
					in[i].LastMessage = c.LastMessage.Format("2006-01-02")
				}
			}
			payload, _ := json.Marshal(in)
			var out classifyOutput
			err := a.complete(ctx, system, "Today is "+time.Now().Format("2006-01-02")+".\n\n"+string(payload), &out)

			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			for _, r := range out.Results {
				results[r.Key] = normalize(r.Analysis)
			}
			done += len(b)
			if progress != nil {
				progress(done, len(chats))
			}
		}(b)
	}
	wg.Wait()
	if firstErr != nil && len(results) == 0 {
		return nil, firstErr
	}
	return results, firstErr
}

func normalize(a store.Analysis) store.Analysis {
	a.Recommendation = strings.ToLower(strings.TrimSpace(a.Recommendation))
	switch a.Recommendation {
	case store.RecKeep, store.RecMute, store.RecArchive, store.RecLeave:
	default:
		a.Recommendation = store.RecKeep
	}
	a.Category = strings.ToLower(strings.TrimSpace(a.Category))
	known := false
	for _, c := range Categories {
		if c == a.Category {
			known = true
			break
		}
	}
	if !known {
		a.Category = "other"
	}
	a.Noise = max(0, min(10, a.Noise))
	return a
}

const overviewSystem = `You write a short, friendly overview of a user's Telegram account to help them clean it up. You get aggregate stats plus a compact list of chats with the AI's per-chat verdicts.

Return ONLY a JSON object:
{"headline":"one punchy sentence","summary":"2-4 sentences describing what their Telegram is mostly made of and where the noise comes from","quick_wins":["3-5 concrete actions, naming specific chats or categories"]}`

// Overview produces an account-level narrative from already-classified chats.
func (a *Analyzer) Overview(ctx context.Context, chats []store.Chat) (*store.Overview, error) {
	type row struct {
		Title string  `json:"t"`
		Kind  string  `json:"k"`
		Cat   string  `json:"c"`
		Rec   string  `json:"r"`
		Noise int     `json:"n"`
		Rate  float64 `json:"mpd"`
	}
	byKind, byCat, byRec := map[string]int{}, map[string]int{}, map[string]int{}
	var rows []row
	for _, c := range chats {
		byKind[string(c.Kind)]++
		if c.Analysis == nil {
			continue
		}
		byCat[c.Analysis.Category]++
		byRec[c.Analysis.Recommendation]++
		rows = append(rows, row{c.Title, string(c.Kind), c.Analysis.Category, c.Analysis.Recommendation, c.Analysis.Noise, c.MsgsPerDay})
	}
	if len(rows) == 0 {
		return nil, errors.New("no analysed chats yet")
	}
	// Noisiest first, capped so huge accounts stay within small context windows.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Noise*100+int(rows[i].Rate) > rows[j].Noise*100+int(rows[j].Rate) })
	if len(rows) > 150 {
		rows = rows[:150]
	}
	payload, _ := json.Marshal(map[string]any{
		"total": len(chats), "by_kind": byKind, "by_category": byCat, "by_recommendation": byRec, "chats": rows,
	})
	var out store.Overview
	if err := a.complete(ctx, overviewSystem, string(payload), &out); err != nil {
		return nil, err
	}
	out.CreatedAt = time.Now()
	return &out, nil
}
