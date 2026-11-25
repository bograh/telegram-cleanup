// Package web serves the HTMX dashboard.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/bograh/telegram-cleanup/internal/ai"
	"github.com/bograh/telegram-cleanup/internal/store"
	"github.com/bograh/telegram-cleanup/internal/telegram"
	"github.com/bograh/telegram-cleanup/internal/views"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	tg      *telegram.Client // nil when Telegram isn't configured
	ai      *ai.Analyzer     // nil when AI isn't configured
	store   *store.Store
	missing []string // missing required config, shown on the setup page
	baseCtx context.Context

	jobMu      sync.Mutex
	job        views.Job
	jobCancel  context.CancelFunc
	jobChanged bool // a job finished and the browser hasn't been told yet

	lastCode telegram.CodeInfo // where the last login code went, for re-rendering
}

func New(ctx context.Context, tg *telegram.Client, an *ai.Analyzer, st *store.Store, missing []string) *Server {
	return &Server{tg: tg, ai: an, store: st, missing: missing, baseCtx: ctx}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /login/phone", s.loginPhone)
	mux.HandleFunc("POST /login/code", s.loginCode)
	mux.HandleFunc("POST /login/password", s.loginPassword)
	mux.HandleFunc("POST /login/resend", s.loginResend)
	mux.HandleFunc("POST /login/qr", s.loginQR)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /scan", s.scan)
	mux.HandleFunc("POST /analyze", s.analyze)
	mux.HandleFunc("GET /overview", s.overview)
	mux.HandleFunc("GET /chats", s.chats)
	mux.HandleFunc("POST /actions", s.actions)
	mux.HandleFunc("POST /bulk", s.bulk)
	mux.HandleFunc("POST /stop", s.stop)
	return sameOrigin(mux)
}

// sameOrigin rejects cross-site POSTs so other websites can't drive the
// dashboard (it holds a live Telegram session).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
			if r.Header.Get("HX-Request") != "true" {
				http.Error(w, "expected an htmx request", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func render(w http.ResponseWriter, r *http.Request, c templ.Component) {
	if err := c.Render(r.Context(), w); err != nil {
		log.Printf("render: %v", err)
	}
}

func (s *Server) authorized(r *http.Request) (bool, error) {
	if s.tg == nil {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	return s.tg.Authorized(ctx)
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	ok, err := s.authorized(r)
	if err != nil || !ok {
		w.Header().Set("HX-Redirect", "/")
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return false
	}
	return true
}

func filterFrom(r *http.Request) views.Filter {
	q := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		q = r.Form
	}
	return views.Filter{Q: q.Get("q"), Kind: q.Get("kind"), Cat: q.Get("cat"), Rec: q.Get("rec"), Sort: q.Get("sort")}
}

func (s *Server) dashboard(f views.Filter) views.Dashboard {
	d := views.Build(s.store.Chats(), f)
	d.Overview = s.store.Overview()
	d.ScannedAt = s.store.ScannedAt()
	d.AIEnabled = s.ai != nil
	if s.ai != nil {
		d.AIModel = s.ai.Model()
	}
	if s.tg != nil {
		d.Me = s.tg.Me()
	}
	s.jobMu.Lock()
	d.Job = s.job
	s.jobMu.Unlock()
	return d
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if len(s.missing) > 0 {
		render(w, r, views.Setup(s.missing))
		return
	}
	ok, err := s.authorized(r)
	if err != nil {
		http.Error(w, "Could not connect to Telegram: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !ok {
		render(w, r, views.LoginPage(views.Login{Step: "phone"}))
		return
	}
	render(w, r, views.DashboardPage(s.dashboard(filterFrom(r))))
}

// --- login ---

func codeStep(info telegram.CodeInfo) views.Login {
	return views.Login{Step: "code", Where: info.Where, Next: info.Next, CanResend: info.CanResend, Blocked: info.Blocked}
}

func (s *Server) loginPhone(w http.ResponseWriter, r *http.Request) {
	phone := strings.ReplaceAll(strings.TrimSpace(r.FormValue("phone")), " ", "")
	info, err := s.tg.SendCode(r.Context(), phone)
	switch {
	case errors.Is(err, telegram.ErrAlreadyAuthorized):
		w.Header().Set("HX-Redirect", "/")
	case err != nil:
		render(w, r, views.LoginForm(views.Login{Step: "phone", Err: friendly(err)}))
	default:
		s.setCodeInfo(info)
		render(w, r, views.LoginForm(codeStep(info)))
	}
}

func (s *Server) loginResend(w http.ResponseWriter, r *http.Request) {
	info, err := s.tg.ResendCode(r.Context())
	switch {
	case errors.Is(err, telegram.ErrAlreadyAuthorized):
		w.Header().Set("HX-Redirect", "/")
	case err != nil:
		l := codeStep(s.codeInfo())
		l.Err = friendly(err)
		render(w, r, views.LoginForm(l))
	default:
		s.setCodeInfo(info)
		l := codeStep(info)
		l.Notice = "Code sent again."
		render(w, r, views.LoginForm(l))
	}
}

func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	err := s.tg.SignIn(r.Context(), r.FormValue("code"))
	switch {
	case errors.Is(err, telegram.ErrPasswordNeeded):
		render(w, r, views.LoginForm(views.Login{Step: "password"}))
	case err != nil:
		l := codeStep(s.codeInfo())
		l.Err = friendly(err)
		render(w, r, views.LoginForm(l))
	default:
		w.Header().Set("HX-Redirect", "/")
	}
}

func (s *Server) loginPassword(w http.ResponseWriter, r *http.Request) {
	if err := s.tg.Password(r.Context(), r.FormValue("password")); err != nil {
		render(w, r, views.LoginForm(views.Login{Step: "password", Err: friendly(err)}))
		return
	}
	w.Header().Set("HX-Redirect", "/")
}

// loginQR shows the QR code and is polled by the page until the phone
// approves the login.
func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	st, err := s.tg.QR(r.Context())
	switch {
	case errors.Is(err, telegram.ErrPasswordNeeded):
		render(w, r, views.LoginForm(views.Login{Step: "password"}))
	case err != nil:
		render(w, r, views.LoginForm(views.Login{Step: "qr", Err: friendly(err)}))
	case st.Done:
		w.Header().Set("HX-Redirect", "/")
	default:
		render(w, r, views.LoginForm(views.Login{Step: "qr", QRImage: st.Image}))
	}
}

func (s *Server) setCodeInfo(i telegram.CodeInfo) {
	s.jobMu.Lock()
	s.lastCode = i
	s.jobMu.Unlock()
}

func (s *Server) codeInfo() telegram.CodeInfo {
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return s.lastCode
}

func friendly(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "API_ID_INVALID"):
		return "Telegram rejected TELEGRAM_APP_ID / TELEGRAM_APP_HASH. Check them at my.telegram.org/apps and restart."
	case strings.Contains(msg, "PHONE_NUMBER_INVALID"):
		return "That phone number isn't valid. Include the country code, e.g. +233…"
	case strings.Contains(msg, "PHONE_CODE_INVALID"):
		return "That code is wrong. Try again."
	case strings.Contains(msg, "PHONE_CODE_EXPIRED"):
		return "The code expired. Reload the page to request a new one."
	case strings.Contains(msg, "PASSWORD_HASH_INVALID"):
		return "Wrong password."
	case strings.Contains(msg, "SEND_CODE_UNAVAILABLE"):
		return "Telegram has used up every way to resend the code. Use QR login, or wait a few minutes and start over."
	case strings.Contains(msg, "AUTH_TOKEN_EXPIRED"):
		return "The QR code expired before it was scanned. A new one is shown."
	case strings.Contains(msg, "FLOOD_WAIT"):
		return "Telegram is rate-limiting login attempts. Wait a bit and try again."
	}
	return msg
}

// --- background jobs ---

func (s *Server) startJob(name, msg string, fn func(ctx context.Context, progress func(done, total int)) error) bool {
	s.jobMu.Lock()
	if s.job.Running {
		s.jobMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.job = views.Job{Name: name, Running: true, Msg: msg}
	s.jobCancel = cancel
	s.jobMu.Unlock()

	go func() {
		defer cancel()
		progress := func(done, total int) {
			s.jobMu.Lock()
			s.job.Done, s.job.Total = done, total
			s.jobMu.Unlock()
		}
		err := fn(ctx, progress)
		s.jobMu.Lock()
		s.job.Running = false
		s.job.Err = ""
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("%s: %v", name, err)
			s.job.Err = err.Error()
		}
		s.jobChanged = true
		s.jobCancel = nil
		s.jobMu.Unlock()
	}()
	return true
}

func (s *Server) setResult(msg string) {
	s.jobMu.Lock()
	s.job.Result = msg
	s.jobMu.Unlock()
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.jobMu.Lock()
	j, changed := s.job, s.jobChanged
	if !j.Running && changed {
		s.jobChanged = false
		s.job.Result = ""
	}
	s.jobMu.Unlock()
	if !j.Running && changed {
		trig := map[string]any{"dataChanged": true}
		if j.Result != "" {
			trig["toast"] = j.Result
		}
		b, _ := json.Marshal(trig)
		w.Header().Set("HX-Trigger", string(b))
	}
	render(w, r, views.JobStatus(j, s.ai != nil, len(s.store.Chats()) > 0))
}

// stop cancels the running job. Work already finished is kept.
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.jobMu.Lock()
	if s.jobCancel != nil && s.job.Running {
		s.jobCancel()
		s.job.Stopping = true
		s.job.Msg = "Stopping…"
	}
	s.jobMu.Unlock()
	s.status(w, r)
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	s.startJob("scan", "Scanning chats", func(ctx context.Context, progress func(int, int)) error {
		chats, err := s.tg.Scan(ctx, progress)
		if ctx.Err() != nil {
			// A partial scan would drop chats from the list, so keep the old one.
			s.setResult("Scan stopped. Your previous results are unchanged.")
			return nil
		}
		if err != nil {
			return err
		}
		return s.store.ReplaceChats(chats)
	})
	s.status(w, r)
}

func (s *Server) analyze(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	if s.ai == nil {
		http.Error(w, "AI is not configured", http.StatusBadRequest)
		return
	}
	all := r.URL.Query().Get("all") == "1"
	s.startJob("analyze", "AI is reading your chats", func(ctx context.Context, progress func(int, int)) error {
		var todo []store.Chat
		for _, c := range s.store.Chats() {
			if c.Kind != store.KindDeleted && (all || c.Analysis == nil) {
				todo = append(todo, c)
			}
		}
		var classifyErr error
		if len(todo) > 0 {
			// Save each batch as it lands so stopping (or a crash) keeps the work.
			saved := 0
			save := func(res map[string]store.Analysis) {
				if err := s.store.SetAnalyses(res); err != nil {
					log.Printf("saving analyses: %v", err)
					return
				}
				saved += len(res)
			}
			res, err := s.ai.Classify(ctx, todo, progress, save)
			if ctx.Err() != nil {
				s.setResult(fmt.Sprintf("AI stopped. %d of %d chats analyzed and saved. Click Analyze to continue.", saved, len(todo)))
				return nil
			}
			if err != nil && len(res) == 0 {
				return err
			}
			classifyErr = err
		}
		s.jobMu.Lock()
		s.job.Msg = "Writing overview"
		s.jobMu.Unlock()
		ov, err := s.ai.Overview(ctx, s.store.Chats())
		if ctx.Err() != nil {
			s.setResult("AI stopped before writing the overview. Chat results are saved.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("overview: %w", err)
		}
		if err := s.store.SetOverview(ov); err != nil {
			return err
		}
		if classifyErr != nil {
			return fmt.Errorf("some chats couldn't be analyzed (run Analyze again to retry): %w", classifyErr)
		}
		return nil
	})
	s.status(w, r)
}

// --- fragments ---

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.OverviewSection(s.dashboard(filterFrom(r))))
}

func (s *Server) chats(w http.ResponseWriter, r *http.Request) {
	render(w, r, views.ChatSection(s.dashboard(filterFrom(r))))
}

// --- actions ---

type actionSpec struct {
	do    func(context.Context, store.Chat) error
	apply func(*store.Chat)
	verb  string
}

func (s *Server) actionFor(action string) (actionSpec, bool) {
	switch action {
	case "leave":
		return actionSpec{s.tg.Leave, func(c *store.Chat) { c.Gone = true }, "Left"}, true
	case "mute":
		return actionSpec{s.tg.Mute, func(c *store.Chat) { c.Muted = true }, "Muted"}, true
	case "archive":
		return actionSpec{s.tg.Archive, func(c *store.Chat) { c.Archived = true }, "Archived"}, true
	case "read":
		return actionSpec{s.tg.MarkRead, func(c *store.Chat) { c.Unread = 0 }, "Marked read"}, true
	}
	return actionSpec{}, false
}

// skip reports whether a chat should be left out of an action, and why.
// Chats you own are never left in bulk: leaving an owned channel or group
// can orphan it.
func skip(action string, c store.Chat, bulk bool) bool {
	switch action {
	case "leave":
		return bulk && c.Owner
	case "mute":
		return c.Muted
	case "archive":
		return c.Archived
	case "read":
		return c.Unread == 0
	}
	return false
}

type tally struct {
	ok, skipped, failed int
	lastErr             error
}

func (t tally) message(verb string) string {
	msg := fmt.Sprintf("%s %d chat%s", verb, t.ok, plural(t.ok))
	if t.skipped > 0 {
		msg += fmt.Sprintf(" · %d skipped", t.skipped)
	}
	if t.failed > 0 {
		msg += fmt.Sprintf(" · %d failed (%v)", t.failed, t.lastErr)
	}
	return msg
}

// run applies spec to each key. pause spaces out requests to stay clear of
// Telegram's flood limits on long bulk runs.
func (s *Server) run(ctx context.Context, action string, spec actionSpec, keys []string, bulk bool, pause time.Duration, progress func(int, int)) tally {
	var t tally
	for i, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if progress != nil {
			progress(i, len(keys))
		}
		c, found := s.store.Get(k)
		if !found || c.Gone || skip(action, c, bulk) {
			t.skipped++
			continue
		}
		if err := spec.do(ctx, c); err != nil {
			if ctx.Err() != nil {
				break
			}
			t.failed++
			t.lastErr = err
			log.Printf("%s %q: %v", action, c.Title, err)
			continue
		}
		_ = s.store.Update(k, spec.apply)
		t.ok++
		if pause > 0 && i < len(keys)-1 {
			select {
			case <-time.After(pause):
			case <-ctx.Done():
			}
		}
	}
	if progress != nil {
		progress(t.ok+t.skipped+t.failed, len(keys))
	}
	return t
}

// actions handles the per-row buttons: one chat, done inline.
func (s *Server) actions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	_ = r.ParseForm()
	action, keys := r.Form.Get("action"), r.Form["keys"]
	spec, ok := s.actionFor(action)
	if !ok {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	t := s.run(r.Context(), action, spec, keys, false, 0, nil)
	toast(w, t.message(spec.verb))
	s.chats(w, r)
}

// bulk runs an action over many chats as a background job with progress,
// so leaving hundreds of channels doesn't hang a browser request.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	_ = r.ParseForm()
	action, keys := r.Form.Get("action"), r.Form["keys"]
	spec, ok := s.actionFor(action)
	if !ok {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if len(keys) == 0 {
		toast(w, "Nothing selected")
		s.status(w, r)
		return
	}
	label := map[string]string{"leave": "Leaving", "mute": "Muting", "archive": "Archiving", "read": "Marking read"}[action]
	started := s.startJob("bulk", fmt.Sprintf("%s %d chat%s", label, len(keys), plural(len(keys))), func(ctx context.Context, progress func(int, int)) error {
		pause := 300 * time.Millisecond
		if action == "leave" {
			pause = 700 * time.Millisecond // leaving is the most rate-limited call
		}
		t := s.run(ctx, action, spec, keys, true, pause, progress)
		msg := t.message(spec.verb)
		if ctx.Err() != nil {
			msg = "Stopped. " + msg
		}
		s.setResult(msg)
		return nil
	})
	if !started {
		toast(w, "Another job is still running. Wait for it to finish.")
	}
	s.status(w, r)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// toast asks the page to show a message and refresh the overview stats.
func toast(w http.ResponseWriter, msg string) {
	b, _ := json.Marshal(map[string]any{"toast": msg, "statsChanged": true})
	w.Header().Set("HX-Trigger", string(b))
}
