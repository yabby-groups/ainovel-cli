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
	userName string
	id       string
	engine   *host.Host
	cancel   context.CancelFunc
	cocreate *cocreateState
	coMu     sync.Mutex
	locked   bool
	busy     bool
	queue    []queuedWrite
}

// queuedWrite deliberately contains no credential or session material. The
// credential is looked up only when the request reaches the front of the queue.
type queuedWrite struct {
	userID   string
	userName string
	action   string
	run      func(*bookRuntime, *host.Host) (any, error)
}

type collaborationState struct {
	Holder   string `json:"holder,omitempty"`
	Running  bool   `json:"running"`
	Queued   int    `json:"queued"`
	Position int    `json:"position,omitempty"`
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
	mux.HandleFunc("/api/books", s.handleBooks)
	handle("/api/books/switch", s.booksSwitch)
	handle("/api/books/queue/cancel", s.cancelQueuedWrite)

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
	var input struct {
		ReturnAfterAuthorization bool `json:"return_after_authorization"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
	}
	result, err := s.auth.startDevice(r.Context(), input.ReturnAfterAuthorization)
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
		var held []*bookRuntime
		for _, runtime := range s.books {
			filtered := runtime.queue[:0]
			for _, item := range runtime.queue {
				if item.userID != u.ID {
					filtered = append(filtered, item)
				}
			}
			runtime.queue = filtered
			if runtime.userID == u.ID {
				held = append(held, runtime)
			}
		}
		delete(s.active, u.ID)
		s.bookMu.Unlock()
		for _, runtime := range held {
			s.releaseWriter(runtime.id, runtime.engine, "持锁用户退出")
		}
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
				s.releaseWriter(bookID, h, "创作轮次结束")
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

// Books are collaborative workspaces. Identity selects the credentials for a
// write turn, not a separate copy of the book.
func (s *server) bookKey(_ string, id string) string { return id }
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
	_ = userID
	return filepath.Join(s.auth.cfg.dataDir, "shared", "books", id, "output", "novel")
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
	_ = userID
	ids := []string{defaultBookID}
	entries, err := os.ReadDir(filepath.Join(s.auth.cfg.dataDir, "shared", "books"))
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

func (s *server) collaborationLocked(userID, id string) collaborationState {
	rt := s.books[s.bookKey("", id)]
	if rt == nil || !rt.locked {
		return collaborationState{}
	}
	state := collaborationState{Holder: rt.userName, Running: rt.engine != nil && rt.engine.Snapshot().IsRunning, Queued: len(rt.queue)}
	for i, item := range rt.queue {
		if item.userID == userID {
			state.Position = i + 1
			break
		}
	}
	return state
}

func (s *server) broadcastCollaborationLocked(bookID string) {
	for _, rt := range s.books {
		if rt.id != bookID {
			continue
		}
		state := collaborationState{Holder: rt.userName, Running: rt.locked && rt.engine != nil && rt.engine.Snapshot().IsRunning, Queued: len(rt.queue)}
		s.broadcastTyped("", bookID, "queue_updated", state)
		return
	}
	s.broadcastTyped("", bookID, "queue_updated", collaborationState{})
}

// withWritingEngine serializes all state-changing work for one shared book.
// A queued request is acknowledged immediately; it is replayed only after it
// becomes the head of the queue and its user's credentials are loaded again.
func (s *server) withWritingEngine(w http.ResponseWriter, r *http.Request, action string, run func(*bookRuntime, *host.Host) (any, error)) {
	u, err := userFrom(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	s.bookMu.Lock()
	id := s.loadActiveBookLocked(r.Context(), u.ID)
	key := s.bookKey("", id)
	rt := s.books[key]
	if rt != nil && rt.locked && rt.userID != u.ID {
		if action == "停止创作" || action == "取消共创" {
			s.bookMu.Unlock()
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "当前由其他协作者写作，只有持锁者可以" + action})
			return
		}
		for i, item := range rt.queue {
			if item.userID == u.ID {
				s.bookMu.Unlock()
				writeJSONStatus(w, http.StatusAccepted, map[string]any{"ok": true, "queued": true, "position": i + 1})
				return
			}
		}
		rt.queue = append(rt.queue, queuedWrite{userID: u.ID, userName: u.Name, action: action, run: run})
		position := len(rt.queue)
		s.broadcastTyped(u.ID, id, "queued", map[string]any{"user": u.Name, "position": position, "action": action})
		s.broadcastCollaborationLocked(id)
		s.bookMu.Unlock()
		writeJSONStatus(w, http.StatusAccepted, map[string]any{"ok": true, "queued": true, "position": position})
		return
	}
	if rt != nil && !rt.locked {
		s.teardownBookLocked(rt.userID, id)
		rt = nil
	}
	if rt == nil {
		rt, err = s.ensureBookLocked(r.Context(), u.ID, id)
		if err != nil {
			s.bookMu.Unlock()
			http.Error(w, "engine unavailable: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	rt.userID, rt.userName, rt.locked = u.ID, u.Name, true
	result, err := run(rt, rt.engine)
	if err == nil {
		s.broadcastTyped(u.ID, id, "lock_acquired", map[string]any{"user": u.Name, "action": action})
		s.broadcastCollaborationLocked(id)
	}
	shouldRelease := err != nil || (!rt.engine.Snapshot().IsRunning && !rt.busy)
	s.bookMu.Unlock()
	if shouldRelease {
		s.releaseWriter(id, rt.engine, map[bool]string{true: "请求失败", false: "请求完成"}[err != nil])
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "data": result})
}

func (s *server) releaseWriter(bookID string, h *host.Host, reason string) {
	s.bookMu.Lock()
	rt := s.books[s.bookKey("", bookID)]
	if rt == nil || !rt.locked || rt.engine != h {
		s.bookMu.Unlock()
		return
	}
	queue := rt.queue
	holder := rt.userName
	s.teardownBookLocked(rt.userID, bookID)
	s.broadcastTyped("", bookID, "released", map[string]any{"user": holder, "reason": reason})
	if len(queue) == 0 {
		s.broadcastCollaborationLocked(bookID)
		s.bookMu.Unlock()
		return
	}
	next := queue[0]
	// Keep the remaining queue in a placeholder until the next Host is ready.
	placeholder := &bookRuntime{id: bookID, queue: queue[1:]}
	s.books[s.bookKey("", bookID)] = placeholder
	s.broadcastCollaborationLocked(bookID)
	s.bookMu.Unlock()
	go s.runQueuedWrite(bookID, next)
}

func (s *server) runQueuedWrite(bookID string, item queuedWrite) {
	s.bookMu.Lock()
	key := s.bookKey("", bookID)
	placeholder := s.books[key]
	if placeholder == nil || placeholder.locked {
		s.bookMu.Unlock()
		return
	}
	queue := placeholder.queue
	delete(s.books, key)
	rt, err := s.ensureBookLocked(context.Background(), item.userID, bookID)
	if err != nil {
		s.books[key] = &bookRuntime{id: bookID, queue: queue}
		s.broadcastTyped(item.userID, bookID, "queue_skipped", map[string]any{"user": item.userName, "action": item.action, "error": err.Error()})
		s.bookMu.Unlock()
		s.releasePlaceholder(bookID)
		return
	}
	rt.userID, rt.userName, rt.locked, rt.queue = item.userID, item.userName, true, queue
	_, err = item.run(rt, rt.engine)
	s.broadcastTyped(item.userID, bookID, "lock_acquired", map[string]any{"user": item.userName, "action": item.action})
	s.broadcastCollaborationLocked(bookID)
	shouldRelease := err != nil || (!rt.engine.Snapshot().IsRunning && !rt.busy)
	h := rt.engine
	s.bookMu.Unlock()
	if err != nil {
		s.broadcastTyped(item.userID, bookID, "queue_skipped", map[string]any{"user": item.userName, "action": item.action, "error": err.Error()})
	}
	if shouldRelease {
		s.releaseWriter(bookID, h, "请求完成")
	}
}

func (s *server) releasePlaceholder(bookID string) {
	s.bookMu.Lock()
	rt := s.books[s.bookKey("", bookID)]
	if rt == nil || rt.locked || len(rt.queue) == 0 {
		s.bookMu.Unlock()
		return
	}
	next := rt.queue[0]
	rt.queue = rt.queue[1:]
	s.bookMu.Unlock()
	go s.runQueuedWrite(bookID, next)
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

// publicBooksListLocked exposes the shared bookshelf without exposing a
// workspace's local runtime details to visitors.
func (s *server) publicBooksListLocked() []bookInfo {
	ids := s.discoverBookIDsLocked("")
	books := make([]bookInfo, 0, len(ids))
	for _, id := range ids {
		books = append(books, bookInfo{ID: id, Title: id, Active: id == defaultBookID})
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
	switch r.Method {
	case http.MethodGet:
		u, err := s.auth.currentUser(r)
		s.bookMu.Lock()
		if err != nil {
			books := s.publicBooksListLocked()
			s.bookMu.Unlock()
			writeJSON(w, map[string]any{"active": defaultBookID, "books": books})
			return
		}
		active := s.loadActiveBookLocked(r.Context(), u.ID)
		books := s.booksListLocked(u.ID)
		collaboration := s.collaborationLocked(u.ID, active)
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"active": active, "books": books, "collaboration": collaboration})
	case http.MethodPost:
		u, err := s.auth.currentUser(r)
		if err != nil {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
			return
		}
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
		collaboration := s.collaborationLocked(u.ID, active)
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"active": active, "books": books, "collaboration": collaboration})
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
	collaboration := s.collaborationLocked(u.ID, active)
	s.bookMu.Unlock()
	writeJSON(w, map[string]any{"active": active, "books": books, "collaboration": collaboration})
}

func (s *server) cancelQueuedWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, err := userFrom(r)
	if err != nil {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	s.bookMu.Lock()
	id := s.loadActiveBookLocked(r.Context(), u.ID)
	rt := s.books[s.bookKey("", id)]
	if rt == nil || !rt.locked {
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "cancelled": false})
		return
	}
	for i, item := range rt.queue {
		if item.userID != u.ID {
			continue
		}
		rt.queue = append(rt.queue[:i], rt.queue[i+1:]...)
		s.broadcastTyped(u.ID, id, "queue_cancelled", map[string]any{"user": u.Name})
		s.broadcastCollaborationLocked(id)
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "cancelled": true})
		return
	}
	s.bookMu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "cancelled": false})
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
	s.withWritingEngine(w, r, "开始创作", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "恢复创作", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "发送干预", func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Steer(text) })
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
	s.withWritingEngine(w, r, "继续创作", func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Continue(text) })
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
	s.withWritingEngine(w, r, "切换验收", func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.SetAdvanceMode(mode) })
}

func (s *server) engineNext(w http.ResponseWriter, r *http.Request) {
	s.withWritingEngine(w, r, "放行下一章", func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.AdvanceOneChapter() })
}

func (s *server) engineStop(w http.ResponseWriter, r *http.Request) {
	s.withWritingEngine(w, r, "停止创作", func(rt *bookRuntime, h *host.Host) (any, error) { return map[string]any{"stopped": h.Abort()}, nil })
}

func (s *server) engineReopen(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withWritingEngine(w, r, "重开创作", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "语义导入", func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.ImportFrom(context.Background(), opts)
		if err != nil {
			return nil, err
		}
		rt.busy = true
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "import", ev)
			}
			s.broadcastTyped(userID, bookID, "import_done", nil)
			s.releaseWriter(bookID, h, "导入结束")
		}()
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineSimulate(w http.ResponseWriter, r *http.Request) {
	s.withWritingEngine(w, r, "仿写画像", func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.Simulate(context.Background())
		if err != nil {
			return nil, err
		}
		rt.busy = true
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "sim", ev)
			}
			s.broadcastTyped(userID, bookID, "sim_done", nil)
			s.releaseWriter(bookID, h, "仿写结束")
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
	s.withWritingEngine(w, r, "导入仿写画像", func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID, userID := rt.id, rt.userID
		ch, err := h.ImportSimulationProfile(context.Background(), strings.TrimSpace(input.Path))
		if err != nil {
			return nil, err
		}
		rt.busy = true
		go func() {
			for ev := range ch {
				s.broadcastTyped(userID, bookID, "sim", ev)
			}
			s.broadcastTyped(userID, bookID, "sim_done", nil)
			s.releaseWriter(bookID, h, "画像导入结束")
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
	s.withWritingEngine(w, r, "同步修订", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "生成诊断", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "切换模型", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "切换推理强度", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "开始共创", func(rt *bookRuntime, h *host.Host) (any, error) {
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
		rt.busy = true
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
	s.withWritingEngine(w, r, "共创对话", func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withWritingEngine(w, r, "应用共创", func(rt *bookRuntime, h *host.Host) (any, error) {
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
			rt.busy = false
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
		rt.busy = false
		return map[string]any{"started": true}, nil
	})
}

func (s *server) cocreateCancel(w http.ResponseWriter, r *http.Request) {
	s.withWritingEngine(w, r, "取消共创", func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		rt.cocreate = nil
		rt.coMu.Unlock()
		rt.busy = false
		h.CancelCoCreate()
		return map[string]any{"cancelled": true}, nil
	})
}

func (s *server) chapters(w http.ResponseWriter, r *http.Request) {
	u, err := s.auth.currentUser(r)
	s.bookMu.Lock()
	var root string
	if err != nil {
		id := strings.TrimSpace(r.URL.Query().Get("book"))
		if id == "" {
			id = defaultBookID
		}
		found := false
		for _, candidate := range s.discoverBookIDsLocked("") {
			if candidate == id {
				found = true
				break
			}
		}
		if !found {
			s.bookMu.Unlock()
			writeJSON(w, []chapter{})
			return
		}
		root = filepath.Join(s.bookDirForID("", id), "chapters")
	} else {
		active := s.loadActiveBookLocked(r.Context(), u.ID)
		root = filepath.Join(s.bookDirForID(u.ID, active), "chapters")
		if rt := s.books[s.bookKey(u.ID, active)]; rt != nil && rt.engine != nil {
			root = filepath.Join(rt.engine.Dir(), "chapters")
		}
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
