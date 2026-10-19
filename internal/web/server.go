// Package web serves the chat UI, JSON APIs and the SSE event stream.
// All routes except /login and /static require the session cookie.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"tgbridge/internal/config"
	"tgbridge/internal/pipeline"
	"tgbridge/internal/store"
)

//go:embed assets
var assets embed.FS

const cookieName = "tgb_session"

// TGAuth is the Telegram capability set implemented by tggw.Gateway.
// Plain strings both ways keep the two packages decoupled.
type TGAuth interface {
	AuthStateInfo() string
	SubmitAuth(action, phone, code, password string) (state, errMsg string)
	MarkRead(ctx context.Context, chat store.Chat, maxTgMsgID int) error
	PeerReadOutbox(ctx context.Context, chat store.Chat) (int, error)
}

type Server struct {
	cfg      config.Server
	store    *store.Store
	pipe     *pipeline.Pipeline
	bus      *pipeline.Bus
	secret   []byte
	tg       TGAuth
	assetVer string // content hash of embedded assets, for cache-busting URLs
}

func New(cfg config.Server, st *store.Store, pipe *pipeline.Pipeline, bus *pipeline.Bus, tg TGAuth) (*Server, error) {
	// Derive the cookie secret from the panel password so sessions
	// survive restarts; a password change invalidates them all.
	sum := sha256.Sum256([]byte("tgbridge-panel:" + cfg.Password))
	ver := md5.Sum(append(readAsset(assets, "assets/static/style.css"), readAsset(assets, "assets/static/app.js")...))
	return &Server{
		cfg: cfg, store: st, pipe: pipe, bus: bus,
		secret: sum[:], tg: tg,
		assetVer: hex.EncodeToString(ver[:])[:8],
	}, nil
}

func readAsset(fs embed.FS, path string) []byte {
	b, err := fs.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// ListenAndServe runs until ctx is done, then drains gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("web listening", "addr", s.cfg.Listen)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /static/", s.handleStatic)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("GET /logout", s.handleLogout)
	a := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAuth(h) }
	mux.HandleFunc("GET /{$}", a(s.handleApp))
	mux.HandleFunc("GET /api/chats", a(s.handleChats))
	mux.HandleFunc("POST /api/chats/{id}/enable", a(s.handleEnable))
	mux.HandleFunc("GET /api/chats/{id}/messages", a(s.handleMessages))
	mux.HandleFunc("POST /api/chats/{id}/send", a(s.handleSend))
	mux.HandleFunc("GET /api/stream", a(s.handleStream))
	mux.HandleFunc("GET /api/stats", a(s.handleStats))
	mux.HandleFunc("GET /api/auth/state", a(s.handleAuthState))
	mux.HandleFunc("POST /api/auth/code", a(s.handleAuthStep("code")))
	mux.HandleFunc("POST /api/auth/verify", a(s.handleAuthStep("verify")))
	mux.HandleFunc("POST /api/auth/password", a(s.handleAuthStep("password")))
	mux.HandleFunc("POST /api/chats/{id}/read", a(s.handleMarkRead))
	mux.HandleFunc("GET /stats", a(s.handleStatsPage))
	return mux
}

// handleMarkRead advances the panel's read watermark, pushes the read
// receipt to Telegram, and returns the peer's read-outbox watermark so
// the UI can draw ✓/✓✓ on outgoing messages.
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad chat id"})
		return
	}
	chat, err := s.store.GetChat(id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "chat not found"})
		return
	}
	rowID, tgMsgID, err := s.store.LatestMessage(id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if rowID > 0 {
		if err := s.store.SetChatReadWatermark(id, rowID); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	readOutbox := 0
	if ob, err := s.tg.PeerReadOutbox(r.Context(), *chat); err == nil {
		readOutbox = ob
		if ob > 0 {
			_ = s.store.SetChatReadOutbox(id, int64(ob))
		}
	} else {
		slog.Warn("read outbox query failed", "chat_id", id, "err", err)
	}
	// Fall back to chat-stored watermark when the live query failed.
	if readOutbox == 0 && chat.ReadOutboxMaxID > 0 {
		readOutbox = int(chat.ReadOutboxMaxID)
	}
	if tgMsgID > 0 || rowID > 0 {
		if err := s.tg.MarkRead(r.Context(), *chat, int(tgMsgID)); err != nil {
			slog.Warn("tg mark read failed", "chat_id", id, "err", err)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "read_outbox": readOutbox})
}

// --- stats page (server-rendered) ---

type statsView struct {
	Calls            int
	PromptTokens     int
	CachedTokens     int
	CompletionTokens int
	HitRate          float64
	P50MS, P95MS     int
	AvgMS            int
	HitRatePct       int
	Kinds            []store.KindStats
	Chats            []store.ChatStats
	Statuses         map[string]int64
	V                string
}

func (s *Server) handleStatsPage(w http.ResponseWriter, r *http.Request) {
	view := statsView{Statuses: map[string]int64{}}
	prompt, cached, completion, calls, err := s.store.CacheStats()
	if err == nil {
		view.Calls = calls
		view.PromptTokens = prompt
		view.CachedTokens = cached
		view.CompletionTokens = completion
		if prompt > 0 {
			view.HitRate = float64(cached) / float64(prompt)
			view.HitRatePct = int(math.Round(view.HitRate * 100))
		}
	}
	if err == nil {
		if kinds, e := s.store.LLMStatsByKind(); e == nil {
			view.Kinds = kinds
		}
		if lat, e := s.store.TranslateLatencies(); e == nil && len(lat) > 0 {
			view.P50MS = percentile(lat, .50)
			view.P95MS = percentile(lat, .95)
			sum := 0
			for _, v := range lat {
				sum += v
			}
			view.AvgMS = sum / len(lat)
		}
		view.Chats, _ = s.store.ChatStatsList()
		view.Statuses, _ = s.store.MessageStatusCounts()
	}

	tpl, err := template.ParseFS(assets, "assets/templates/stats.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	view.V = s.assetVer
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.Execute(w, view); err != nil {
		slog.Error("stats render", "err", err)
	}
}

func percentile(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(math.Round(float64(len(sorted)-1)*p))]
}

// --- web-driven Telegram login ---

func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": s.tg.AuthStateInfo()})
}

func (s *Server) handleAuthStep(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Phone    string `json:"phone"`
			Code     string `json:"code"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]string{"error": "请求体格式不对"})
			return
		}
		state, errMsg := s.tg.SubmitAuth(action, body.Phone, body.Code, body.Password)
		writeJSON(w, 200, map[string]string{"status": state, "error": errMsg})
	}
}

// --- auth ---

func (s *Server) issueSession() string {
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	msg := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(msg))
	return msg + "." + hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) sessionValid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(parts[0]))
	return hmac.Equal(mac.Sum(nil), decodeHex(parts[1]))
}

func decodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sessionValid(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"unauthorized"}`)
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	s.writeAsset(w, r, "assets/templates/login.html", "text/html; charset=utf-8")
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if subtleEqual(r.FormValue("password"), s.cfg.Password) {
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: s.issueSession(), Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	slog.Warn("login failed", "remote", r.RemoteAddr)
	time.Sleep(500 * time.Millisecond) // crude brute-force damping
	http.Redirect(w, r, "/login?e=1", http.StatusFound)
}

func subtleEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}

// --- static & app shell ---

var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
}

func (s *Server) writeAsset(w http.ResponseWriter, r *http.Request, path, fallbackType string) {
	b, err := assets.ReadFile(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ct := fallbackType
	for ext, t := range contentTypes {
		if strings.HasSuffix(path, ext) {
			ct = t
			break
		}
	}
	// Standard caching: hashed-asset ETag with a short max-age; HTML is
	// always revalidated so a redeploy is picked up immediately.
	etag := fmt.Sprintf(`"%x"`, md5.Sum(b))
	w.Header().Set("ETag", etag)
	if strings.HasSuffix(path, ".html") {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "max-age=300")
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.writeAsset(w, r, "assets/static/"+name, "application/octet-stream")
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	tpl, err := template.ParseFS(assets, "assets/templates/index.html")
	if err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := tpl.Execute(w, map[string]string{"V": s.assetVer}); err != nil {
		slog.Error("index render", "err", err)
	}
}

// --- JSON APIs ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.store.ListChats()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// Panels should show names, not raw IDs: kick off profile lookups
	// for chats that still lack them.
	s.pipe.TryResolvePeers(chats)
	type chatOut struct {
		ID          int64  `json:"id"`
		TgUserID    int64  `json:"tg_user_id"`
		Username    string `json:"username"`
		Title       string `json:"title"`
		Enabled     bool   `json:"enabled"`
		Unread      int    `json:"unread"`
		LastSnippet string `json:"last_snippet"`
		LastTime    string `json:"last_time"`
	}
	out := make([]chatOut, 0, len(chats))
	for _, c := range chats {
		out = append(out, chatOut{c.ID, c.TgUserID, c.Username, c.Title, c.Enabled, c.Unread, c.LastSnippet, c.LastTime})
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleEnable(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad chat id"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	if err := s.store.SetChatEnabled(id, body.Enabled); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.bus.Publish(pipeline.Event{Type: "chats"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad chat id"})
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	msgs, err := s.store.ListMessages(id, before, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	count, _ := s.store.CountMessages(id)
	out := make([]pipeline.MsgPayload, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, pipeline.PayloadFromMessage(m))
	}
	writeJSON(w, 200, map[string]any{"messages": out, "total": count})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad chat id"})
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Text) == "" {
		writeJSON(w, 400, map[string]string{"error": "text required"})
		return
	}
	res := s.pipe.SendOutgoing(r.Context(), id, body.Text)
	if res.Err != nil {
		slog.Error("send outgoing failed", "chat_id", id, "err", res.Err)
		writeJSON(w, 502, map[string]string{"error": res.Err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"row_id": res.RowID, "msg_id": res.MsgID, "target": res.Target})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	prompt, cached, completion, calls, err := s.store.CacheStats()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	hit := 0.0
	if prompt > 0 {
		hit = float64(cached) / float64(prompt)
	}
	writeJSON(w, 200, map[string]any{
		"prompt_tokens": prompt, "cached_tokens": cached,
		"completion_tokens": completion, "calls": calls, "cache_hit_rate": hit,
	})
}

// --- SSE ---

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Reverse proxies must not buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, unsub := s.bus.Subscribe()
	defer unsub()
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if b, err := json.Marshal(ev); err == nil {
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
