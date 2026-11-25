package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitIdle blocks until no job is running or queued.
func waitIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.jobMu.Lock()
		idle := !s.job.Running && len(s.queue) == 0
		s.jobMu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("jobs never finished")
}

func TestJobsQueueInOrder(t *testing.T) {
	s := newTestServer(t)
	release := make(chan struct{})
	var order []string
	job := func(name string, err error) jobFunc {
		return func(context.Context, func(int, int)) error {
			order = append(order, name)
			s.setResult(name + " done")
			return err
		}
	}

	if place, ok := s.startJob("scan", "Scanning", func(ctx context.Context, p func(int, int)) error {
		<-release
		return job("scan", nil)(ctx, p)
	}); place != 0 || !ok {
		t.Fatalf("first job: place=%d ok=%v", place, ok)
	}
	if place, ok := s.startJob("analyze", "Analyzing", job("analyze", errors.New("boom"))); place != 1 || !ok {
		t.Fatalf("second job: place=%d ok=%v", place, ok)
	}
	if _, ok := s.startJob("analyze", "Analyzing", job("analyze", nil)); ok {
		t.Fatal("a duplicate analyze job was queued")
	}
	if place, ok := s.startJob("bulk", "Muting 2 chats", job("bulk-1", nil)); place != 2 || !ok {
		t.Fatalf("bulk job: place=%d ok=%v", place, ok)
	}
	if place, _ := s.startJob("bulk", "Archiving 3 chats", job("bulk-2", nil)); place != 3 {
		t.Fatalf("second bulk job should queue too, place=%d", place)
	}

	// Drop the first bulk job before it runs.
	s.jobMu.Lock()
	id := s.queue[1].id
	s.jobMu.Unlock()
	s.cancelQueued(newRecorder(), newPost("/queue/cancel", "id", id))

	close(release)
	waitIdle(t, s)

	want := []string{"scan", "analyze", "bulk-2"}
	if len(order) != len(want) {
		t.Fatalf("ran %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("ran %v, want %v", order, want)
		}
	}
	// Every finished job's message reaches the page, including the failure
	// of a job that was followed by another.
	s.jobMu.Lock()
	notes, err := s.notes, s.job.Err
	s.jobMu.Unlock()
	wantNotes := []string{"scan done", "analyze done", "Analyzing failed: boom", "bulk-2 done"}
	if len(notes) != len(wantNotes) {
		t.Fatalf("notes = %q, want %q", notes, wantNotes)
	}
	for i := range wantNotes {
		if notes[i] != wantNotes[i] {
			t.Fatalf("notes = %q, want %q", notes, wantNotes)
		}
	}
	if err != "" {
		t.Fatalf("last job succeeded but error line shows %q", err)
	}
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newPost(path, key string, val int) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{key: {strconv.Itoa(val)}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}
