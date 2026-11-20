package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bograh/telegram-cleanup/internal/store"
)

// fakeOpenAI echoes a verdict for every key it receives, wrapped in a code
// fence to make sure extractJSON copes with chatty models.
func fakeOpenAI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "bad request", 400)
			return
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		user := req.Messages[1].Content
		var content string
		if strings.Contains(req.Messages[0].Content, "overview") {
			content = `{"headline":"Mostly crypto noise","summary":"s","quick_wins":["leave X"]}`
		} else {
			var in []chatInput
			_ = json.Unmarshal([]byte(user[strings.Index(user, "["):]), &in)
			var parts []string
			for _, c := range in {
				parts = append(parts, fmt.Sprintf(`{"key":%q,"category":"Crypto & Trading","summary":"x","recommendation":"LEAVE","reason":"spam","noise":14}`, c.Key))
			}
			content = "```json\n{\"results\":[" + strings.Join(parts, ",") + "]}\n```"
		}
		resp := map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}, "finish_reason": "stop"}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestClassifyAndOverview(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL + "/v1/", APIKey: "k", Model: "m", BatchSize: 2, Workers: 2})

	var chats []store.Chat
	for i := range 5 {
		chats = append(chats, store.Chat{Key: fmt.Sprintf("channel:%d", i), Kind: store.KindChannel, Title: "c"})
	}
	res, err := a.Classify(context.Background(), chats, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 5 {
		t.Fatalf("got %d results, want 5", len(res))
	}
	got := res["channel:3"]
	if got.Recommendation != store.RecLeave || got.Category != "crypto & trading" || got.Noise != 10 {
		t.Fatalf("not normalized: %+v", got)
	}

	for i := range chats {
		r := res[chats[i].Key]
		chats[i].Analysis = &r
	}
	ov, err := a.Overview(context.Background(), chats)
	if err != nil {
		t.Fatal(err)
	}
	if ov.Headline != "Mostly crypto noise" || len(ov.QuickWins) != 1 {
		t.Fatalf("bad overview: %+v", ov)
	}
}

func TestNormalizeUnknowns(t *testing.T) {
	got := normalize(store.Analysis{Category: "weird", Recommendation: "delete", Noise: -3})
	if got.Category != "other" || got.Recommendation != store.RecKeep || got.Noise != 0 {
		t.Fatalf("got %+v", got)
	}
}

// groqLike mimics Groq's free tier: any request whose max_tokens exceeds the
// output-tokens-per-minute limit is rejected with 429 "Request too large",
// and replies come wrapped in Qwen3-style <think> blocks.
func groqLike(t *testing.T, limit int, maxSeen *int, requests *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		*requests++
		requested := req.MaxTokens
		if requested == 0 {
			requested = 2048 // provider default when max_tokens is omitted
		}
		if requested > limit {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			fmt.Fprintf(w, `{"error":{"message":"Request too large for model qwen on output tokens per minute (OTPM): Limit %d, Requested %d. The request's expected output tokens exceed the enforced limit; reduce max_tokens"}}`, limit, requested)
			return
		}
		*maxSeen = max(*maxSeen, requested)
		user := req.Messages[1].Content
		var content string
		if strings.Contains(req.Messages[0].Content, "overview") {
			content = `{"headline":"h","summary":"s","quick_wins":[]}`
		} else {
			var in []chatInput
			_ = json.Unmarshal([]byte(user[strings.Index(user, "["):]), &in)
			if len(in)*tokensPerChat+100 > requested {
				t.Errorf("batch of %d sent with only %d max_tokens", len(in), requested)
			}
			var parts []string
			for _, c := range in {
				parts = append(parts, fmt.Sprintf(`{"key":%q,"category":"news","summary":"x","recommendation":"keep","reason":"r","noise":1}`, c.Key))
			}
			content = "<think>Let me look at {these} chats.</think>\n{\"results\":[" + strings.Join(parts, ",") + "]}"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}, "finish_reason": "stop"}}})
	}))
}

func TestAdaptsToOutputTokenLimit(t *testing.T) {
	var maxSeen, requests int
	srv := groqLike(t, 1000, &maxSeen, &requests)
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL, Model: "qwen", BatchSize: 25, Workers: 1})

	var chats []store.Chat
	for i := range 30 {
		chats = append(chats, store.Chat{Key: fmt.Sprintf("channel:%d", i), Title: "c"})
	}
	res, err := a.Classify(context.Background(), chats, nil)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(res) != 30 {
		t.Fatalf("got %d results, want 30", len(res))
	}
	if maxSeen > 1000 {
		t.Fatalf("accepted request with max_tokens %d > limit", maxSeen)
	}
	if _, err := a.Overview(context.Background(), chatsWith(chats, res)); err != nil {
		t.Fatalf("overview: %v", err)
	}
	t.Logf("%d HTTP requests for 30 chats + overview", requests)
}

func chatsWith(chats []store.Chat, res map[string]store.Analysis) []store.Chat {
	for i := range chats {
		r := res[chats[i].Key]
		chats[i].Analysis = &r
	}
	return chats
}

func TestMaxTokensCapAvoidsTooLargeUpFront(t *testing.T) {
	var maxSeen, requests int
	srv := groqLike(t, 1000, &maxSeen, &requests)
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL, Model: "qwen", BatchSize: 25, Workers: 1, MaxTokens: 900})
	var chats []store.Chat
	for i := range 20 {
		chats = append(chats, store.Chat{Key: fmt.Sprintf("channel:%d", i), Title: "c"})
	}
	if _, err := a.Classify(context.Background(), chats, nil); err != nil {
		t.Fatal(err)
	}
	// 20 chats need ~1900 tokens; with a 900 cap they must be split before
	// sending, so no request should have been rejected.
	if requests != 4 {
		t.Fatalf("made %d requests, want 4 (batches of 5 chats)", requests)
	}
}

func TestExtractJSONStripsThinking(t *testing.T) {
	got := extractJSON("<think>maybe {a} or {b}</think>\n```json\n{\"x\":1}\n```")
	if got != `{"x":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestRateLimitHonoursRetryAfter(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0.05")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached for model, please try again in 50ms"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": `{"headline":"h","summary":"s","quick_wins":[]}`}}}})
	}))
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL, Model: "m"})
	chats := []store.Chat{{Key: "k", Title: "t", Analysis: &store.Analysis{Category: "news", Recommendation: "keep"}}}
	start := time.Now()
	if _, err := a.Overview(context.Background(), chats); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || time.Since(start) > 2*time.Second {
		t.Fatalf("calls=%d elapsed=%v: should retry once after ~50ms", calls, time.Since(start))
	}
}
