// Package telegram wraps a gotd/td user-account client: login, dialog scan and
// the cleanup actions (leave, mute, archive, mark read).
package telegram

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/bograh/telegram-cleanup/internal/store"
)

// ErrPasswordNeeded means the account has 2FA and SignIn must be followed by Password.
var ErrPasswordNeeded = errors.New("two-step verification password required")

// Client is a long-running Telegram connection.
type Client struct {
	tc    *telegram.Client
	api   *tg.Client
	ready chan struct{}
	err   error

	mu        sync.Mutex
	phone     string
	codeHash  string
	authed    bool
	selfTitle string

	// QR login: Telegram pushes updateLoginToken when the phone accepts the
	// QR code; loggedIn latches that signal.
	loggedIn qrlogin.LoggedIn
	qrToken  qrlogin.Token

	// onWait is told when Telegram rate-limits us (until is when calls
	// resume) and again with a zero time once the wait is over.
	onWait func(until time.Time)
}

// OnRateLimit registers a callback for Telegram FLOOD_WAIT pauses, so a
// long wait can be shown instead of looking like a stalled job.
func (c *Client) OnRateLimit(fn func(until time.Time)) {
	c.mu.Lock()
	c.onWait = fn
	c.mu.Unlock()
}

func (c *Client) waiting(until time.Time) {
	c.mu.Lock()
	fn := c.onWait
	c.mu.Unlock()
	if fn != nil {
		fn(until)
	}
}

// New starts the MTProto connection in the background. The session is stored
// in sessionPath so you only log in once.
func New(ctx context.Context, appID int, appHash, sessionPath string) *Client {
	c := &Client{ready: make(chan struct{})}
	dispatcher := tg.NewUpdateDispatcher()
	c.loggedIn = qrlogin.OnLoginToken(dispatcher)
	c.tc = telegram.NewClient(appID, appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
		UpdateHandler:  dispatcher,
	})
	go func() {
		c.err = c.tc.Run(ctx, func(ctx context.Context) error {
			c.api = c.tc.API()
			if st, err := c.tc.Auth().Status(ctx); err == nil && st.Authorized {
				c.setAuthed(st.User)
			}
			close(c.ready)
			<-ctx.Done()
			return ctx.Err()
		})
		select {
		case <-c.ready:
		default:
			close(c.ready) // unblock waiters; c.err explains why
		}
	}()
	return c
}

func (c *Client) wait(ctx context.Context) error {
	select {
	case <-c.ready:
		if c.api == nil {
			return fmt.Errorf("telegram connection failed: %w", c.err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) setAuthed(u *tg.User) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authed = true
	if u != nil {
		c.selfTitle = strings.TrimSpace(u.FirstName + " " + u.LastName)
	}
}

// Authorized reports whether a logged-in session exists.
func (c *Client) Authorized(ctx context.Context) (bool, error) {
	if err := c.wait(ctx); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authed, nil
}

// Me returns the logged-in user's display name.
func (c *Client) Me() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selfTitle
}

// Password completes 2FA login.
func (c *Client) Password(ctx context.Context, pw string) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	a, err := c.tc.Auth().Password(ctx, pw)
	if err != nil {
		return err
	}
	u, _ := a.User.(*tg.User)
	c.setAuthed(u)
	return nil
}

// retry runs fn, sleeping through FLOOD_WAIT errors and reporting each wait.
func (c *Client) retry(ctx context.Context, fn func() error) error {
	for {
		err := fn()
		if err == nil {
			return nil
		}
		d, ok := tgerr.AsFloodWait(err)
		if !ok {
			return err
		}
		d += time.Second
		c.waiting(time.Now().Add(d))
		t := time.NewTimer(d)
		select {
		case <-t.C:
			c.waiting(time.Time{})
		case <-ctx.Done():
			t.Stop()
			c.waiting(time.Time{})
			return ctx.Err()
		}
	}
}

// Progress is called during a scan with (done, total).
type Progress func(done, total int)

// scanWorkers is how many chats have their history sampled at once.
const scanWorkers = 4

// Scan lists every channel, group and bot, plus 1:1 chats with deleted
// accounts (other personal chats are skipped), then samples the latest
// messages of each to estimate activity. Chats in prev whose newest message
// hasn't changed reuse their earlier sample instead of fetching it again.
func (c *Client) Scan(ctx context.Context, prev map[string]store.Chat, progress Progress) ([]*store.Chat, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	type item struct {
		chat *store.Chat
		peer tg.InputPeerClass
	}
	var items []item
	now := time.Now()

	err := query.GetDialogs(c.api).BatchSize(100).ForEach(ctx, func(ctx context.Context, e dialogs.Elem) error {
		dlg, ok := e.Dialog.(*tg.Dialog)
		if !ok {
			return nil
		}
		ch := &store.Chat{
			Unread:     dlg.UnreadCount,
			Archived:   dlg.FolderID == 1,
			TopMessage: dlg.TopMessage,
		}
		if mu, ok := dlg.NotifySettings.GetMuteUntil(); ok && int64(mu) > now.Unix() {
			ch.Muted = true
		}
		if e.Last != nil {
			ch.LastMessage = time.Unix(int64(e.Last.GetDate()), 0)
		}
		switch p := dlg.Peer.(type) {
		case *tg.PeerUser:
			u, ok := e.Entities.Users()[p.UserID]
			if !ok || (!u.Bot && !u.Deleted) {
				return nil // skip personal chats with real people
			}
			ch.PeerType, ch.PeerID, ch.AccessHash = store.PeerUser, u.ID, u.AccessHash
			if u.Deleted {
				ch.Kind, ch.Title = store.KindDeleted, "Deleted Account"
				break
			}
			ch.Kind = store.KindBot
			ch.Title = strings.TrimSpace(u.FirstName + " " + u.LastName)
			ch.Username = u.Username
			ch.Verified, ch.ScamFlag = u.Verified, u.Scam || u.Fake
		case *tg.PeerChat:
			g, ok := e.Entities.Chats()[p.ChatID]
			if !ok || g.Deactivated || g.Left {
				return nil
			}
			ch.Kind, ch.PeerType = store.KindGroup, store.PeerChat
			ch.PeerID, ch.Title = g.ID, g.Title
			ch.Members, ch.Owner = g.ParticipantsCount, g.Creator
		case *tg.PeerChannel:
			g, ok := e.Entities.Channels()[p.ChannelID]
			if !ok || g.Left {
				return nil
			}
			ch.Kind = store.KindGroup
			if g.Broadcast {
				ch.Kind = store.KindChannel
			}
			ch.PeerType, ch.PeerID, ch.AccessHash = store.PeerChannel, g.ID, g.AccessHash
			ch.Title, ch.Username = g.Title, g.Username
			ch.Owner, ch.Verified, ch.ScamFlag = g.Creator, g.Verified, g.Scam || g.Fake
			if n, ok := g.GetParticipantsCount(); ok {
				ch.Members = n
			}
		default:
			return nil
		}
		ch.Key = fmt.Sprintf("%s:%d", ch.PeerType, ch.PeerID)
		items = append(items, item{chat: ch, peer: e.Peer})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing dialogs: %w", err)
	}

	// Work out which chats need their history fetched.
	var todo []item
	for _, it := range items {
		ch := it.chat
		if ch.Kind == store.KindDeleted {
			continue // personal history: don't read it, store it or send it to the AI
		}
		if p, ok := prev[ch.Key]; ok && p.Sampled && p.TopMessage == ch.TopMessage {
			ch.Samples, ch.Sampled, ch.SampleCount, ch.SampleSince = p.Samples, true, p.SampleCount, p.SampleSince
			ch.MsgsPerDay = msgsPerDay(ch.SampleCount, ch.SampleSince, now)
			continue
		}
		todo = append(todo, it)
	}

	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
		jobs = make(chan item)
	)
	if progress != nil {
		progress(0, len(todo))
	}
	for range min(scanWorkers, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				// A failed sample just leaves the chat without snippets; it's
				// fetched again on the next scan.
				_ = c.sample(ctx, it.chat, it.peer, now)
				mu.Lock()
				done++
				if progress != nil {
					progress(done, len(todo))
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, it := range todo {
		select {
		case jobs <- it:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	out := make([]*store.Chat, len(items))
	for i, it := range items {
		out[i] = it.chat
	}
	return out, nil
}

// msgsPerDay estimates activity from count messages posted since oldest.
func msgsPerDay(count int, oldest, now time.Time) float64 {
	if count < 2 {
		return 0
	}
	span := now.Sub(oldest).Hours() / 24
	span = math.Max(span, 1.0/24)
	return math.Round(float64(count)/span*10) / 10
}

// sample fetches the latest messages to get text snippets and a msgs/day rate.
func (c *Client) sample(ctx context.Context, ch *store.Chat, peer tg.InputPeerClass, now time.Time) error {
	var res tg.MessagesMessagesClass
	err := c.retry(ctx, func() (err error) {
		res, err = c.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 15})
		return err
	})
	if err != nil {
		return err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil
	}
	var oldest int
	count := 0
	ch.Samples = nil
	for _, m := range mod.GetMessages() {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		count++
		oldest = msg.Date
		if len(ch.Samples) < 6 {
			text := strings.Join(strings.Fields(msg.Message), " ")
			if text == "" && msg.Media != nil {
				text = "[media]"
			}
			if text != "" {
				ch.Samples = append(ch.Samples, truncate(text, 220))
			}
		}
	}
	ch.Sampled, ch.SampleCount = true, count
	if count > 0 {
		ch.SampleSince = time.Unix(int64(oldest), 0)
	}
	ch.MsgsPerDay = msgsPerDay(count, ch.SampleSince, now)
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func inputPeer(ch store.Chat) tg.InputPeerClass {
	switch ch.PeerType {
	case store.PeerUser:
		return &tg.InputPeerUser{UserID: ch.PeerID, AccessHash: ch.AccessHash}
	case store.PeerChat:
		return &tg.InputPeerChat{ChatID: ch.PeerID}
	default:
		return &tg.InputPeerChannel{ChannelID: ch.PeerID, AccessHash: ch.AccessHash}
	}
}

// Leave leaves a channel/group. For bots it blocks the bot and deletes the
// chat; for deleted accounts it just deletes the chat.
func (c *Client) Leave(ctx context.Context, ch store.Chat) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.retry(ctx, func() error {
		switch ch.PeerType {
		case store.PeerChannel:
			_, err := c.api.ChannelsLeaveChannel(ctx, &tg.InputChannel{ChannelID: ch.PeerID, AccessHash: ch.AccessHash})
			return err
		case store.PeerChat:
			_, err := c.api.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
				ChatID: ch.PeerID, UserID: &tg.InputUserSelf{},
			})
			return err
		default:
			peer := inputPeer(ch)
			if ch.Kind == store.KindDeleted {
				return c.deleteHistory(ctx, peer)
			}
			if _, err := c.api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peer}); err != nil {
				return err
			}
			_, err := c.api.MessagesDeleteHistory(ctx, &tg.MessagesDeleteHistoryRequest{Peer: peer})
			return err
		}
	})
}

// deleteHistory removes a 1:1 chat from the dialog list. Telegram deletes long
// histories in chunks and reports a non-zero offset until it's done.
func (c *Client) deleteHistory(ctx context.Context, peer tg.InputPeerClass) error {
	for {
		res, err := c.api.MessagesDeleteHistory(ctx, &tg.MessagesDeleteHistoryRequest{Peer: peer})
		if err != nil {
			return err
		}
		if res.Offset == 0 {
			return nil
		}
	}
}

// Mute silences notifications forever.
func (c *Client) Mute(ctx context.Context, ch store.Chat) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.retry(ctx, func() error {
		_, err := c.api.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
			Peer:     &tg.InputNotifyPeer{Peer: inputPeer(ch)},
			Settings: tg.InputPeerNotifySettings{MuteUntil: math.MaxInt32},
		})
		return err
	})
}

// Archive moves the chat into the Archived folder.
func (c *Client) Archive(ctx context.Context, ch store.Chat) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.retry(ctx, func() error {
		_, err := c.api.FoldersEditPeerFolders(ctx, []tg.InputFolderPeer{{Peer: inputPeer(ch), FolderID: 1}})
		return err
	})
}

// MarkRead clears the unread counter.
func (c *Client) MarkRead(ctx context.Context, ch store.Chat) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.retry(ctx, func() error {
		if ch.PeerType == store.PeerChannel {
			_, err := c.api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
				Channel: &tg.InputChannel{ChannelID: ch.PeerID, AccessHash: ch.AccessHash},
				MaxID:   ch.TopMessage,
			})
			return err
		}
		_, err := c.api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: inputPeer(ch), MaxID: ch.TopMessage})
		return err
	})
}
