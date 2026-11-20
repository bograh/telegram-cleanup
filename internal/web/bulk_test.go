package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bograh/telegram-cleanup/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chats := []*store.Chat{
		{Key: "channel:1", Kind: store.KindChannel, Title: "spam"},
		{Key: "channel:2", Kind: store.KindChannel, Title: "mine", Owner: true},
		{Key: "chat:3", Kind: store.KindGroup, Title: "group", Muted: true},
		{Key: "user:4", Kind: store.KindBot, Title: "bot"},
	}
	if err := st.ReplaceChats(chats); err != nil {
		t.Fatal(err)
	}
	return New(context.Background(), nil, nil, st, nil)
}

func keys() []string { return []string{"channel:1", "channel:2", "chat:3", "user:4", "missing:9"} }

func TestBulkLeaveSkipsOwnedChats(t *testing.T) {
	s := newTestServer(t)
	var called []string
	spec := actionSpec{
		do:    func(_ context.Context, c store.Chat) error { called = append(called, c.Key); return nil },
		apply: func(c *store.Chat) { c.Gone = true },
		verb:  "Left",
	}
	got := s.run(context.Background(), "leave", spec, keys(), true, 0, nil)
	if got.ok != 3 || got.skipped != 2 || got.failed != 0 {
		t.Fatalf("tally = %+v", got)
	}
	for _, k := range called {
		if k == "channel:2" {
			t.Fatal("left a chat the user owns")
		}
	}
	if left := s.store.Chats(); len(left) != 1 || left[0].Key != "channel:2" {
		t.Fatalf("remaining chats = %+v", left)
	}
	if msg := got.message("Left"); msg != "Left 3 chats · 2 skipped" {
		t.Fatalf("message = %q", msg)
	}
}

func TestSingleLeaveAllowsOwnedChat(t *testing.T) {
	s := newTestServer(t)
	spec := actionSpec{do: func(context.Context, store.Chat) error { return nil }, apply: func(c *store.Chat) { c.Gone = true }}
	if got := s.run(context.Background(), "leave", spec, []string{"channel:2"}, false, 0, nil); got.ok != 1 {
		t.Fatalf("tally = %+v", got)
	}
}

func TestBulkMuteSkipsAlreadyMutedAndCountsFailures(t *testing.T) {
	s := newTestServer(t)
	spec := actionSpec{
		do: func(_ context.Context, c store.Chat) error {
			if c.Kind == store.KindBot {
				return errors.New("PEER_ID_INVALID")
			}
			return nil
		},
		apply: func(c *store.Chat) { c.Muted = true },
	}
	got := s.run(context.Background(), "mute", spec, keys(), true, 0, nil)
	if got.ok != 2 || got.skipped != 2 || got.failed != 1 {
		t.Fatalf("tally = %+v", got)
	}
	if msg := got.message("Muted"); !strings.Contains(msg, "1 failed (PEER_ID_INVALID)") {
		t.Fatalf("message = %q", msg)
	}
}

func TestBulkStopsWhenCancelled(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	spec := actionSpec{
		do:    func(context.Context, store.Chat) error { n++; cancel(); return nil },
		apply: func(c *store.Chat) { c.Archived = true },
	}
	got := s.run(ctx, "archive", spec, keys(), true, 0, nil)
	if n != 1 || got.ok != 1 {
		t.Fatalf("ran %d actions after cancel, tally = %+v", n, got)
	}
}
