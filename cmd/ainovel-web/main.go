package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/diag"
	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/host"
	"github.com/voocel/ainovel-cli/internal/host/exp"
	"github.com/voocel/ainovel-cli/internal/host/imp"
	"github.com/voocel/ainovel-cli/internal/store"
)

//go:embed public
var publicFS embed.FS

type chapter struct {
	Number   int       `json:"number"`
	Name     string    `json:"name"`
	Content  string    `json:"content"`
	Modified time.Time `json:"modified"`
}

type cocreateState struct {
	session *startup.CoCreateSession
	stage   bool
}

type bookRuntime struct {
	userID   string
	id       string
	engine   *host.Host
	cancel   context.CancelFunc
	cocreate *cocreateState
	coMu     sync.Mutex
}

type bookInfo struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Dir     string `json:"dir"`
	Active  bool   `json:"active"`
	Running bool   `json:"running"`
}

type server struct {
	auth    *authService
	mu      sync.Mutex
	log     []userEvent
	clients map[chan string]string

	bookMu sync.Mutex
	books  map[string]*bookRuntime
	active map[string]string
}

type userEvent struct {
	userID  string
	payload string
}

const stageCoCreateOpener = "我先暂停一下，想和你一起规划接下来的走向。"

const (
	defaultBookID = "default"
	booksDirName  = "books"
)

func main() {
	authCfg, err := loadAuthConfig()
	if err != nil {
		panic("ainovel-web configuration: " + err.Error())
	}
	auth, err := newAuthService(authCfg)
	if err != nil {
		panic("ainovel-web storage: " + err.Error())
	}
	defer auth.close()
	s := &server{auth: auth, clients: make(map[chan string]string), books: make(map[string]*bookRuntime), active: make(map[string]string)}

	static, _ := fs.Sub(publicFS, "public")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/auth/device", s.authDevice)
	mux.HandleFunc("/api/auth/device/", s.authDeviceStatus)
	mux.HandleFunc("/api/auth/me", s.authMe)
	mux.HandleFunc("/api/auth/logout", s.authLogout)
	handle := func(pattern string, handler http.HandlerFunc) { mux.Handle(pattern, auth.require(handler)) }
	mux.HandleFunc("/api/status", s.status)
	mux.HandleFunc("/api/chapters", s.chapters)
	mux.HandleFunc("/api/events", s.events)
	handle("/api/snapshot", s.snapshot)
	handle("/api/generate", s.engineStart)
	handle("/api/engine/models", s.engineModels)
	handle("/api/engine/start", s.engineStart)
	handle("/api/engine/resume", s.engineResume)
	handle("/api/engine/steer", s.engineSteer)
	handle("/api/engine/continue", s.engineContinue)
	handle("/api/engine/review", s.engineReview)
	handle("/api/engine/next", s.engineNext)
	handle("/api/engine/stop", s.engineStop)
	handle("/api/engine/reopen", s.engineReopen)
	handle("/api/engine/export", s.engineExport)
	handle("/api/engine/import", s.engineImport)
	handle("/api/engine/simulate", s.engineSimulate)
	handle("/api/engine/importsim", s.engineImportSim)
	handle("/api/engine/sync", s.engineSync)
	handle("/api/engine/diag", s.engineDiag)
	handle("/api/engine/model", s.engineModel)
	handle("/api/engine/thinking", s.engineThinking)
	handle("/api/engine/cocreate/start", s.cocreateStart)
	handle("/api/engine/cocreate/send", s.cocreateSend)
	handle("/api/engine/cocreate/apply", s.cocreateApply)
	handle("/api/engine/cocreate/cancel", s.cocreateCancel)
	handle("/api/books", s.handleBooks)
	handle("/api/books/switch", s.booksSwitch)

	addr := os.Getenv("AINOVEL_WEB_ADDR")
	if addr == "" {
		addr = "127.0.0.1:4788"
	}
	fmt.Printf("ainovel-web: http://%s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		panic(err)
	}
}

func (s *server) authDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := s.auth.startDevice(r.Context())
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, result)
}

func (s *server) authDeviceStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/auth/device/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "invalid authorization attempt", http.StatusBadRequest)
		return
	}
	user, err := s.auth.pollDevice(r.Context(), id)
	if err != nil {
		if err.Error() == "authorization_pending" || err.Error() == "slow_down" {
			writeJSON(w, map[string]any{"status": err.Error()})
			return
		}
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	token, err := s.auth.createSession(r.Context(), user)
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "could not create session"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.auth.cfg.secureCookie, MaxAge: 30 * 24 * 60 * 60})
	writeJSON(w, map[string]any{"status": "approved", "user": map[string]string{"id": user.ID, "name": user.Name}})
}

func (s *server) authMe(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.currentUser(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, map[string]any{"authenticated": true, "user": map[string]string{"id": u.ID, "name": u.Name}})
}

func (s *server) authLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if u, err := s.auth.currentUser(r); err == nil {
		s.bookMu.Lock()
		for key, runtime := range s.books {
			if runtime.userID == u.ID {
				if runtime.cancel != nil {
					runtime.cancel()
				}
				if runtime.engine != nil {
					runtime.engine.Close()
				}
				delete(s.books, key)
			}
		}
		delete(s.active, u.ID)
		s.bookMu.Unlock()
	}
	s.auth.logout(r.Context(), r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1, Secure: s.auth.cfg.secureCookie, SameSite: http.SameSiteLaxMode})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.currentUser(r)
	if err != nil {
		writeJSON(w, map[string]any{"configured": false, "events": 0, "active": defaultBookID})
		return
	}
	s.bookMu.Lock()
	active := s.loadActiveBookLocked(r.Context(), u.ID)
	rt := s.books[s.bookKey(u.ID, active)]
	configured := rt != nil && rt.engine != nil
	dir := ""
	if configured {
		dir = "ready"
	}
	s.bookMu.Unlock()

	s.mu.Lock()
	n := 0
	for _, event := range s.log {
		if event.userID == u.ID {
			n++
		}
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"configured": configured, "dir": dir, "events": n, "active": active})
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.currentUser(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}
	ch := make(chan string, 64)
	s.mu.Lock()
	for _, event := range s.log {
		if event.userID == u.ID {
			fmt.Fprintf(w, "data: %s\n\n", event.payload)
		}
	}
	s.clients[ch] = u.ID
	s.mu.Unlock()
	flusher.Flush()
	defer func() { s.mu.Lock(); delete(s.clients, ch); s.mu.Unlock() }()
	for {
		select {
		case event := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", event)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *server) broadcastTyped(userID, bookID, typ string, data any) {
	payload := map[string]any{"type": typ, "book": bookID}
	if data != nil {
		payload["data"] = data
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.log = append(s.log, userEvent{userID: userID, payload: string(b)})
	if len(s.log) > 300 {
		s.log = s.log[len(s.log)-300:]
	}
	for ch, clientUserID := range s.clients {
		if clientUserID != userID {
			continue
		}
		select {
		case ch <- string(b):
		default:
		}
	}
	s.mu.Unlock()
}

func (s *server) pump(ctx context.Context, userID, bookID string, h *host.Host) {
	events := h.Events()
	stream := h.Stream()
	done := h.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
			} else {
				s.broadcastTyped(userID, bookID, "event", ev)
			}
		case delta, ok := <-stream:
			if !ok {
				stream = nil
			} else if delta == host.StreamClearSentinel {
				s.broadcastTyped(userID, bookID, "clear", nil)
			} else {
				s.broadcastTyped(userID, bookID, "stream", delta)
			}
		case _, ok := <-done:
			if !ok {
				done = nil
			} else {
				s.broadcastTyped(userID, bookID, "done", nil)
			}
		}
	}
}

func (s *server) currentConfig(ctx context.Context, userID string) (bootstrap.Config, error) {
	cred, err := s.auth.credential(ctx, userID)
	if err != nil {
		return bootstrap.Config{}, fmt.Errorf("read user credential: %w", err)
	}
	availableModels, err := s.auth.models(ctx, userID)
	if err != nil {
		return bootstrap.Config{}, fmt.Errorf("load Myna models: %w", err)
	}
	model := "gpt-5.6-luna"
	models := make([]bootstrap.ModelConfig, 0, len(availableModels))
	for _, name := range availableModels {
		models = append(models, bootstrap.ModelConfig{Name: name})
	}
	if !containsModel(availableModels, model) {
		model = availableModels[0]
	}
	cfg := bootstrap.Config{Provider: "myna", ModelName: model, Style: "default", Providers: map[string]bootstrap.ProviderConfig{"myna": {Type: "openai", APIKey: cred.APIKey, BaseURL: s.auth.cfg.apiBase, Models: models}}, Roles: map[string]bootstrap.RoleConfig{}}
	cfg.FillDefaults()
	return cfg, nil
}

func containsModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

func (s *server) bookKey(userID, id string) string { return userID + "\x00" + id }
func (s *server) activeBook(userID string) string {
	if id := s.active[userID]; id != "" {
		return id
	}
	return defaultBookID
}

func (s *server) loadActiveBookLocked(ctx context.Context, userID string) string {
	if id := s.active[userID]; id != "" {
		return id
	}
	var id string
	_ = s.auth.db.QueryRowContext(ctx, `SELECT active_book FROM preferences WHERE user_id=?`, userID).Scan(&id)
	if strings.TrimSpace(id) == "" {
		id = defaultBookID
	}
	for _, candidate := range s.discoverBookIDsLocked(userID) {
		if candidate == id {
			s.active[userID] = id
			return id
		}
	}
	s.active[userID] = defaultBookID
	return defaultBookID
}

func (s *server) saveActiveBook(ctx context.Context, userID, bookID string) {
	_, _ = s.auth.db.ExecContext(ctx, `UPDATE preferences SET active_book=?,updated_at=? WHERE user_id=?`, bookID, time.Now().Unix(), userID)
}

func (s *server) bookDirForID(userID, id string) string {
	safeUserID, err := safeBookID(userID)
	if err != nil {
		return ""
	}
	return filepath.Join(s.auth.cfg.dataDir, "users", safeUserID, "books", id, "output", "novel")
}

func safeBookID(name string) (string, error) {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			b.WriteByte('_')
		default:
			if r < 32 {
				b.WriteByte('_')
			} else {
				b.WriteRune(r)
			}
		}
	}
	id := strings.Trim(strings.TrimSpace(b.String()), ". ")
	if id == "" || id == "." || id == ".." {
		return "", fmt.Errorf("book name is empty")
	}
	if len([]rune(id)) > 64 {
		runes := []rune(id)
		id = string(runes[:64])
	}
	return id, nil
}

func (s *server) discoverBookIDsLocked(userID string) []string {
	ids := []string{defaultBookID}
	safeUserID, err := safeBookID(userID)
	if err != nil {
		return ids
	}
	entries, err := os.ReadDir(filepath.Join(s.auth.cfg.dataDir, "users", safeUserID, "books"))
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() != defaultBookID {
				ids = append(ids, entry.Name())
			}
		}
	}
	sort.Strings(ids[1:])
	return ids
}

func (s *server) ensureBookLocked(ctx context.Context, userID, id string) (*bookRuntime, error) {
	key := s.bookKey(userID, id)
	if rt, ok := s.books[key]; ok {
		return rt, nil
	}
	cfg, err := s.currentConfig(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}
	cfg.OutputDir = s.bookDirForID(userID, id)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, err
	}
	bundle := assets.Load(cfg.Style, assets.DefaultLoadOptions(cfg.OutputDir))
	eng, err := host.New(cfg, bundle)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &bookRuntime{userID: userID, id: id, engine: eng, cancel: cancel}
	s.books[key] = rt
	go s.pump(ctx, userID, id, eng)
	return rt, nil
}

func (s *server) teardownBookLocked(userID, id string) {
	key := s.bookKey(userID, id)
	rt := s.books[key]
	if rt == nil {
		return
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.engine != nil {
		rt.engine.Close()
	}
	delete(s.books, key)
}

func (s *server) withEngine(w http.ResponseWriter, r *http.Request, fn func(rt *bookRuntime, h *host.Host) (any, error)) {
	u, err := userFrom(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	s.bookMu.Lock()
	defer s.bookMu.Unlock()
	active := s.loadActiveBookLocked(r.Context(), u.ID)
	rt, err := s.ensureBookLocked(r.Context(), u.ID, active)
	if err != nil {
		http.Error(w, "engine unavailable: "+err.Error(), 400)
		return
	}
	result, err := fn(rt, rt.engine)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "data": result})
}

func (s *server) booksListLocked(userID string) []bookInfo {
	ids := s.discoverBookIDsLocked(userID)
	active := s.activeBook(userID)
	books := make([]bookInfo, 0, len(ids))
	for _, id := range ids {
		info := bookInfo{ID: id, Title: id, Active: id == active}
		if rt := s.books[s.bookKey(userID, id)]; rt != nil && rt.engine != nil {
			info.Dir = rt.engine.Dir()
			info.Running = rt.engine.Snapshot().IsRunning
		}
		books = append(books, info)
	}
	return books
}

func (s *server) uniqueBookIDLocked(userID, base string) string {
	candidate := base
	for i := 2; ; i++ {
		exists := false
		for _, id := range s.discoverBookIDsLocked(userID) {
			if id == candidate {
				exists = true
				break
			}
		}
		if !exists {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

func (s *server) handleBooks(w http.ResponseWriter, r *http.Request) {
	u, err := userFrom(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.bookMu.Lock()
		active := s.loadActiveBookLocked(r.Context(), u.ID)
		books := s.booksListLocked(u.ID)
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"active": active, "books": books})
	case http.MethodPost:
		var input struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid json", 400)
			return
		}
		base, err := safeBookID(input.Name)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		s.bookMu.Lock()
		id := s.uniqueBookIDLocked(u.ID, base)
		s.active[u.ID] = id
		s.saveActiveBook(r.Context(), u.ID, id)
		if _, err := s.ensureBookLocked(r.Context(), u.ID, id); err != nil {
			s.bookMu.Unlock()
			http.Error(w, err.Error(), 400)
			return
		}
		books := s.booksListLocked(u.ID)
		active := s.loadActiveBookLocked(r.Context(), u.ID)
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"active": active, "books": books})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *server) booksSwitch(w http.ResponseWriter, r *http.Request) {
	u, err := userFrom(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var input struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	id := strings.TrimSpace(input.ID)
	if id == "" {
		id = defaultBookID
	}
	s.bookMu.Lock()
	found := false
	for _, candidate := range s.discoverBookIDsLocked(u.ID) {
		if candidate == id {
			found = true
			break
		}
	}
	if !found {
		s.bookMu.Unlock()
		http.Error(w, "book not found", 404)
		return
	}
	if _, err := s.ensureBookLocked(r.Context(), u.ID, id); err != nil {
		s.bookMu.Unlock()
		http.Error(w, "engine unavailable: "+err.Error(), 400)
		return
	}
	s.active[u.ID] = id
	s.saveActiveBook(r.Context(), u.ID, id)
	books := s.booksListLocked(u.ID)
	active := s.loadActiveBookLocked(r.Context(), u.ID)
	s.bookMu.Unlock()
	writeJSON(w, map[string]any{"active": active, "books": books})
}
func (s *server) snapshot(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return h.Snapshot(), nil })
}

func (s *server) engineModels(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		roles := []string{"default", "architect", "writer", "editor"}
		providers := h.ConfiguredProviders()
		providerModels := map[string][]string{}
		for _, p := range providers {
			providerModels[p] = h.ConfiguredModels(p)
		}
		roleModels := map[string]any{}
		for _, role := range roles {
			provider, model, ok := h.CurrentModelSelection(role)
			levels := []string{}
			for _, l := range h.AvailableThinking(role) {
				levels = append(levels, string(l))
			}
			roleModels[role] = map[string]any{
				"provider":           provider,
				"model":              model,
				"ok":                 ok,
				"thinking":           h.CurrentThinking(role),
				"available_thinking": levels,
			}
		}
		return map[string]any{
			"providers":       providers,
			"provider_models": providerModels,
			"roles":           roleModels,
		}, nil
	})
}

func (s *server) engineStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	prompt := strings.TrimSpace(input.Prompt)
	if prompt == "" {
		http.Error(w, "prompt is required", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		prepared, err := startup.PrepareQuick(prompt)
		if err != nil {
			return nil, err
		}
		if err := h.PrepareUserRules(prepared); err != nil {
			return nil, err
		}
		if err := h.StartPrepared(prepared); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineResume(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		label, err := h.Resume()
		if err != nil {
			return nil, err
		}
		if label == "" {
			return nil, fmt.Errorf("no resumable session")
		}
		return map[string]any{"label": label}, nil
	})
}

func (s *server) engineSteer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	text := strings.TrimSpace(input.Text)
	if text == "" {
		http.Error(w, "text is required", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Steer(text) })
}

func (s *server) engineContinue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	text := strings.TrimSpace(input.Text)
	if text == "" {
		http.Error(w, "text is required", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Continue(text) })
}

func (s *server) engineReview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	mode := domain.ChapterAdvanceReview
	if input.Mode == "off" {
		mode = domain.ChapterAdvanceAuto
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.SetAdvanceMode(mode) })
}

func (s *server) engineNext(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.AdvanceOneChapter() })
}

func (s *server) engineStop(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return map[string]any{"stopped": h.Abort()}, nil })
}

func (s *server) engineReopen(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		return nil, h.Reopen(strings.TrimSpace(input.Direction))
	})
}

func (s *server) engineExport(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path      string `json:"path"`
		Format    string `json:"format"`
		From      int    `json:"from"`
		To        int    `json:"to"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	if strings.TrimSpace(input.Path) != "" {
		http.Error(w, "server filesystem export paths are disabled", 400)
		return
	}
	opts := exp.Options{OutPath: input.Path, From: input.From, To: input.To, Overwrite: input.Overwrite}
	if input.Format != "" {
		opts.Format = exp.Format(input.Format)
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) { return h.Export(context.Background(), opts) })
}

func (s *server) engineImport(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path            string `json:"path"`
		AutoConfirm     bool   `json:"auto_confirm"`
		StoryResolution string `json:"story_resolution"`
		ContinueAfter   bool   `json:"continue_after"`
		Guidance        string `json:"guidance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	if strings.TrimSpace(input.Path) != "" {
		http.Error(w, "server filesystem imports are disabled", 400)
		return
	}
	opts := imp.Options{
		SourcePath:      input.Path,
		AutoConfirm:     input.AutoConfirm,
		StoryResolution: input.StoryResolution,
		ContinueAfter:   input.ContinueAfter,
		Guidance:        input.Guidance,
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.ImportFrom(context.Background(), opts)
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "import", ev)
			}
			s.broadcastTyped(userID, bookID, "import_done", nil)
		}()
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineSimulate(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.Simulate(context.Background())
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "sim", ev)
			}
			s.broadcastTyped(userID, bookID, "sim_done", nil)
		}()
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineImportSim(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	if strings.TrimSpace(input.Path) != "" {
		http.Error(w, "server filesystem imports are disabled", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.ImportSimulationProfile(context.Background(), strings.TrimSpace(input.Path))
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "sim", ev)
			}
			s.broadcastTyped(userID, bookID, "sim_done", nil)
		}()
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineSync(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Check bool `json:"check"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		if input.Check {
			nums, err := h.CheckChapterRevisions()
			if err != nil {
				return nil, err
			}
			return map[string]any{"changed": nums}, nil
		}
		res, err := h.SyncChapterRevisions(context.Background())
		return res, err
	})
}

func (s *server) engineDiag(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		path, err := diag.Export(store.NewStore(h.Dir()))
		if err != nil {
			return nil, err
		}
		return map[string]any{"generated": path != ""}, nil
	})
}

func (s *server) engineModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Role     string `json:"role"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		if err := h.SwitchModel(input.Role, input.Provider, input.Model); err != nil {
			return nil, err
		}
		return map[string]any{"role": input.Role, "provider": input.Provider, "model": input.Model}, nil
	})
}

func (s *server) engineThinking(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Role  string `json:"role"`
		Level string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		if err := h.SetRoleThinking(input.Role, input.Level); err != nil {
			return nil, err
		}
		return map[string]any{"role": input.Role, "level": input.Level}, nil
	})
}

func (s *server) cocreateStart(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Stage   bool   `json:"stage"`
		Initial string `json:"initial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		defer rt.coMu.Unlock()
		if input.Stage {
			if !h.PauseForCoCreate() {
				return nil, fmt.Errorf("cannot enter stage cocreate")
			}
			rt.cocreate = &cocreateState{session: startup.NewCoCreateSession(stageCoCreateOpener), stage: true}
		} else {
			rt.cocreate = &cocreateState{session: startup.NewCoCreateSession(input.Initial), stage: false}
		}
		return map[string]any{"stage": input.Stage}, nil
	})
}

func (s *server) cocreateSend(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		state := rt.cocreate
		if state == nil {
			state = &cocreateState{session: startup.NewCoCreateSession(input.Text), stage: false}
			rt.cocreate = state
		} else if strings.TrimSpace(input.Text) != "" {
			state.session.AppendUser(strings.TrimSpace(input.Text))
		}
		rt.coMu.Unlock()

		stream := h.CoCreateStream
		if state.stage {
			stream = h.StageCoCreateStream
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		reply, err := stream(ctx, state.session.History(), func(kind, text string) {
			s.broadcastTyped(rt.userID, rt.id, "cocreate_delta", map[string]any{"kind": kind, "text": text})
		})
		if err != nil {
			return nil, err
		}

		rt.coMu.Lock()
		state.session.ApplyReply(reply)
		rt.coMu.Unlock()

		return map[string]any{
			"message":     reply.Message,
			"prompt":      reply.Prompt,
			"ready":       reply.Ready,
			"suggestions": reply.Suggestions,
			"stage":       state.stage,
		}, nil
	})
}

func (s *server) cocreateApply(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Draft string `json:"draft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		state := rt.cocreate
		rt.coMu.Unlock()
		if state == nil {
			return nil, fmt.Errorf("no active cocreate")
		}
		draft := strings.TrimSpace(input.Draft)
		if draft == "" {
			draft = state.session.DraftPrompt()
		}
		if draft == "" {
			return nil, fmt.Errorf("draft is required")
		}
		if state.stage {
			return map[string]any{"stage": true}, h.ResumeFromCoCreate(draft)
		}
		prepared, err := startup.PrepareQuick(draft)
		if err != nil {
			return nil, err
		}
		if err := h.PrepareUserRules(prepared); err != nil {
			return nil, err
		}
		if err := h.StartPrepared(prepared); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	})
}

func (s *server) cocreateCancel(w http.ResponseWriter, r *http.Request) {
	s.withEngine(w, r, func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		rt.cocreate = nil
		rt.coMu.Unlock()
		h.CancelCoCreate()
		return map[string]any{"cancelled": true}, nil
	})
}

func (s *server) chapters(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.currentUser(r)
	if err != nil {
		writeJSON(w, []chapter{})
		return
	}
	s.bookMu.Lock()
	active := s.loadActiveBookLocked(r.Context(), u.ID)
	root := filepath.Join(s.bookDirForID(u.ID, active), "chapters")
	if rt := s.books[s.bookKey(u.ID, active)]; rt != nil && rt.engine != nil {
		root = filepath.Join(rt.engine.Dir(), "chapters")
	}
	s.bookMu.Unlock()

	entries, err := os.ReadDir(root)
	if err != nil {
		writeJSON(w, []chapter{})
		return
	}
	result := make([]chapter, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
		if readErr != nil {
			continue
		}
		info, _ := entry.Info()
		name := strings.TrimSuffix(entry.Name(), ".md")
		result = append(result, chapter{Name: name, Content: string(data), Modified: info.ModTime()})
	}
	// 文件修改时间记录章节的实际生成/提交时间；最新生成的章节置顶。
	// 同一时间戳下按名称排序，保证刷新列表时顺序稳定。
	sort.Slice(result, func(i, j int) bool {
		if result[i].Modified.Equal(result[j].Modified) {
			return result[i].Name < result[j].Name
		}
		return result[i].Modified.After(result[j].Modified)
	})
	writeJSON(w, result)
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
