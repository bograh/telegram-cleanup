package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
