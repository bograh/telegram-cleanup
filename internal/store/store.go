// Package store keeps the scanned chats and AI analysis on disk as JSON so the
// dashboard survives restarts without rescanning Telegram.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Kind is the coarse chat type shown in the dashboard.
type Kind string

const (
	KindChannel Kind = "channel"
	KindGroup   Kind = "group"
	KindBot     Kind = "bot"
	// KindDeleted is a 1:1 chat with an account that no longer exists.
	KindDeleted Kind = "deleted"
)

// PeerType says how to rebuild the Telegram InputPeer for actions.
type PeerType string

const (
	PeerUser    PeerType = "user"
	PeerChat    PeerType = "chat"
	PeerChannel PeerType = "channel"
)

// Recommendation values the AI may return.
const (
	RecKeep    = "keep"
	RecMute    = "mute"
	RecArchive = "archive"
	RecLeave   = "leave"
)

// Analysis is Claude's verdict on one chat.
type Analysis struct {
	Category       string `json:"category"`
	Summary        string `json:"summary"`
	Recommendation string `json:"recommendation"`
	Reason         string `json:"reason"`
	Noise          int    `json:"noise"` // 0-10, how spammy/noisy
}

// Chat is one channel, group or bot from the user's dialog list.
type Chat struct {
	Key        string   `json:"key"`
	Kind       Kind     `json:"kind"`
	Title      string   `json:"title"`
	Username   string   `json:"username,omitempty"`
	PeerType   PeerType `json:"peer_type"`
	PeerID     int64    `json:"peer_id"`
	AccessHash int64    `json:"access_hash"`
	TopMessage int      `json:"top_message"`

	Members     int       `json:"members"`
	Unread      int       `json:"unread"`
	Muted       bool      `json:"muted"`
	Archived    bool      `json:"archived"`
	Owner       bool      `json:"owner"`
	Verified    bool      `json:"verified"`
	ScamFlag    bool      `json:"scam_flag"`
	LastMessage time.Time `json:"last_message"`
	MsgsPerDay  float64   `json:"msgs_per_day"`
	Samples     []string  `json:"samples,omitempty"`

	Analysis *Analysis `json:"analysis,omitempty"`
	// Gone is set after the user left/deleted the chat from the dashboard.
	Gone bool `json:"gone,omitempty"`
}

// Overview is the AI's top-level narrative for the whole account.
type Overview struct {
	Headline  string    `json:"headline"`
	Summary   string    `json:"summary"`
	QuickWins []string  `json:"quick_wins"`
	CreatedAt time.Time `json:"created_at"`
}

type state struct {
	Chats     []*Chat   `json:"chats"`
	Overview  *Overview `json:"overview,omitempty"`
	ScannedAt time.Time `json:"scanned_at"`
}

// Store is a mutex-guarded JSON file.
type Store struct {
	mu   sync.RWMutex
	path string
	st   state
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ReplaceChats stores a fresh scan, carrying over existing analyses for chats
// that are still present so re-scanning doesn't cost another AI run.
func (s *Store) ReplaceChats(chats []*Chat) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := make(map[string]*Analysis, len(s.st.Chats))
	for _, c := range s.st.Chats {
		if c.Analysis != nil {
			old[c.Key] = c.Analysis
		}
	}
	for _, c := range chats {
		if a, ok := old[c.Key]; ok {
			c.Analysis = a
		}
	}
	sort.SliceStable(chats, func(i, j int) bool { return chats[i].MsgsPerDay > chats[j].MsgsPerDay })
	s.st.Chats = chats
	s.st.ScannedAt = time.Now()
	return s.saveLocked()
}

// Chats returns copies of all chats that haven't been removed.
func (s *Store) Chats() []Chat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Chat, 0, len(s.st.Chats))
	for _, c := range s.st.Chats {
		if !c.Gone {
			out = append(out, *c)
		}
	}
	return out
}

func (s *Store) Get(key string) (Chat, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.st.Chats {
		if c.Key == key {
			return *c, true
		}
	}
	return Chat{}, false
}

// Update applies fn to the chat with key and persists.
func (s *Store) Update(key string, fn func(*Chat)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.st.Chats {
		if c.Key == key {
			fn(c)
			return s.saveLocked()
		}
	}
	return nil
}

func (s *Store) SetAnalyses(m map[string]Analysis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.st.Chats {
		if a, ok := m[c.Key]; ok {
			a := a
			c.Analysis = &a
		}
	}
	return s.saveLocked()
}

func (s *Store) SetOverview(o *Overview) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Overview = o
	return s.saveLocked()
}

func (s *Store) Overview() *Overview {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Overview
}

func (s *Store) ScannedAt() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.ScannedAt
}
