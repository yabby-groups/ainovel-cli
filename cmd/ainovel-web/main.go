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
	mu      sync.Mutex
	log     []string
	clients map[chan string]struct{}

	bookMu sync.Mutex
	books  map[string]*bookRuntime
	active string
}

const stageCoCreateOpener = "我先暂停一下，想和你一起规划接下来的走向。"

const (
	defaultBookID = "default"
	booksDirName  = "books"
)

func main() {
	s := &server{clients: make(map[chan string]struct{}), books: make(map[string]*bookRuntime), active: defaultBookID}
	if err := s.reloadAllBooks(); err != nil {
		fmt.Fprintf(os.Stderr, "engine not ready: %v\n", err)
	}

	static, _ := fs.Sub(publicFS, "public")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/status", s.status)
	mux.HandleFunc("/api/chapters", s.chapters)
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/config", s.config)
	mux.HandleFunc("/api/snapshot", s.snapshot)
	mux.HandleFunc("/api/generate", s.engineStart)
	mux.HandleFunc("/api/engine/models", s.engineModels)
	mux.HandleFunc("/api/engine/start", s.engineStart)
	mux.HandleFunc("/api/engine/resume", s.engineResume)
	mux.HandleFunc("/api/engine/steer", s.engineSteer)
	mux.HandleFunc("/api/engine/continue", s.engineContinue)
	mux.HandleFunc("/api/engine/review", s.engineReview)
	mux.HandleFunc("/api/engine/next", s.engineNext)
	mux.HandleFunc("/api/engine/stop", s.engineStop)
	mux.HandleFunc("/api/engine/reopen", s.engineReopen)
	mux.HandleFunc("/api/engine/export", s.engineExport)
	mux.HandleFunc("/api/engine/import", s.engineImport)
	mux.HandleFunc("/api/engine/simulate", s.engineSimulate)
	mux.HandleFunc("/api/engine/importsim", s.engineImportSim)
	mux.HandleFunc("/api/engine/sync", s.engineSync)
	mux.HandleFunc("/api/engine/diag", s.engineDiag)
	mux.HandleFunc("/api/engine/model", s.engineModel)
	mux.HandleFunc("/api/engine/thinking", s.engineThinking)
	mux.HandleFunc("/api/engine/cocreate/start", s.cocreateStart)
	mux.HandleFunc("/api/engine/cocreate/send", s.cocreateSend)
	mux.HandleFunc("/api/engine/cocreate/apply", s.cocreateApply)
	mux.HandleFunc("/api/engine/cocreate/cancel", s.cocreateCancel)
	mux.HandleFunc("/api/books", s.handleBooks)
	mux.HandleFunc("/api/books/switch", s.booksSwitch)

	port := os.Getenv("AINOVEL_WEB_PORT")
	if port == "" {
		port = "4788"
	}
	fmt.Printf("ainovel-web: http://127.0.0.1:%s\n", port)
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		panic(err)
	}
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	s.bookMu.Lock()
	rt := s.books[s.active]
	configured := rt != nil && rt.engine != nil
	dir := ""
	if configured {
		dir = rt.engine.Dir()
	}
	active := s.active
	s.bookMu.Unlock()

	s.mu.Lock()
	n := len(s.log)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"configured": configured, "dir": dir, "events": n, "active": active})
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
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
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
	s.clients[ch] = struct{}{}
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

func (s *server) broadcast(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, event)
	if len(s.log) > 300 {
		s.log = s.log[len(s.log)-300:]
	}
	for ch := range s.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (s *server) broadcastTyped(bookID, typ string, data any) {
	payload := map[string]any{"type": typ, "book": bookID}
	if data != nil {
		payload["data"] = data
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.broadcast(string(b))
}

func (s *server) pump(ctx context.Context, bookID string, h *host.Host) {
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
				s.broadcastTyped(bookID, "event", ev)
			}
		case delta, ok := <-stream:
			if !ok {
				stream = nil
			} else if delta == host.StreamClearSentinel {
				s.broadcastTyped(bookID, "clear", nil)
			} else {
				s.broadcastTyped(bookID, "stream", delta)
			}
		case _, ok := <-done:
			if !ok {
				done = nil
			} else {
				s.broadcastTyped(bookID, "done", nil)
			}
		}
	}
}

func (s *server) currentConfig() (bootstrap.Config, error) {
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return cfg, err
	}
	cfg.FillDefaults()
	return cfg, nil
}

func (s *server) bookDirForID(id string) string {
	if id == defaultBookID {
		return filepath.Join("output", "novel")
	}
	return filepath.Join(booksDirName, id, "output", "novel")
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

func (s *server) discoverBookIDsLocked() []string {
	ids := []string{defaultBookID}
	entries, err := os.ReadDir(booksDirName)
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

func (s *server) ensureBookLocked(id string) (*bookRuntime, error) {
	if rt, ok := s.books[id]; ok {
		return rt, nil
	}
	cfg, err := s.currentConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateBase(); err != nil {
		return nil, err
	}
	cfg.OutputDir = s.bookDirForID(id)
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return nil, err
	}
	bundle := assets.Load(cfg.Style, assets.DefaultLoadOptions(cfg.OutputDir))
	eng, err := host.New(cfg, bundle)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &bookRuntime{id: id, engine: eng, cancel: cancel}
	s.books[id] = rt
	go s.pump(ctx, id, eng)
	return rt, nil
}

func (s *server) teardownBookLocked(id string) {
	rt := s.books[id]
	if rt == nil {
		return
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.engine != nil {
		rt.engine.Close()
	}
	delete(s.books, id)
}

func (s *server) reloadAllBooks() error {
	s.bookMu.Lock()
	defer s.bookMu.Unlock()
	for id := range s.books {
		s.teardownBookLocked(id)
	}
	ids := s.discoverBookIDsLocked()
	var firstErr error
	for _, id := range ids {
		if _, err := s.ensureBookLocked(id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if _, ok := s.books[s.active]; !ok {
		if _, err := s.ensureBookLocked(s.active); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *server) withEngine(w http.ResponseWriter, fn func(rt *bookRuntime, h *host.Host) (any, error)) {
	s.bookMu.Lock()
	defer s.bookMu.Unlock()
	rt, err := s.ensureBookLocked(s.active)
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

func (s *server) booksListLocked() []bookInfo {
	ids := s.discoverBookIDsLocked()
	books := make([]bookInfo, 0, len(ids))
	for _, id := range ids {
		info := bookInfo{ID: id, Title: id, Active: id == s.active}
		if rt := s.books[id]; rt != nil && rt.engine != nil {
			info.Dir = rt.engine.Dir()
			info.Running = rt.engine.Snapshot().IsRunning
		}
		books = append(books, info)
	}
	return books
}

func (s *server) uniqueBookIDLocked(base string) string {
	candidate := base
	for i := 2; ; i++ {
		exists := false
		for _, id := range s.discoverBookIDsLocked() {
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
		s.bookMu.Lock()
		active := s.active
		books := s.booksListLocked()
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
		id := s.uniqueBookIDLocked(base)
		s.active = id
		if _, err := s.ensureBookLocked(id); err != nil {
			s.bookMu.Unlock()
			http.Error(w, err.Error(), 400)
			return
		}
		books := s.booksListLocked()
		active := s.active
		s.bookMu.Unlock()
		writeJSON(w, map[string]any{"active": active, "books": books})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *server) booksSwitch(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := s.books[id]; !ok {
		s.bookMu.Unlock()
		http.Error(w, "book not found", 404)
		return
	}
	s.active = id
	books := s.booksListLocked()
	active := s.active
	s.bookMu.Unlock()
	writeJSON(w, map[string]any{"active": active, "books": books})
}
func (s *server) snapshot(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return h.Snapshot(), nil })
}

func (s *server) engineModels(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
		return map[string]any{"dir": h.Dir()}, nil
	})
}

func (s *server) engineResume(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		label, err := h.Resume()
		if err != nil {
			return nil, err
		}
		if label == "" {
			return nil, fmt.Errorf("no resumable session")
		}
		return map[string]any{"label": label, "dir": h.Dir()}, nil
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Steer(text) })
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.Continue(text) })
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.SetAdvanceMode(mode) })
}

func (s *server) engineNext(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return nil, h.AdvanceOneChapter() })
}

func (s *server) engineStop(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return map[string]any{"stopped": h.Abort()}, nil })
}

func (s *server) engineReopen(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
	opts := exp.Options{OutPath: input.Path, From: input.From, To: input.To, Overwrite: input.Overwrite}
	if input.Format != "" {
		opts.Format = exp.Format(input.Format)
	}
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) { return h.Export(context.Background(), opts) })
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
	opts := imp.Options{
		SourcePath:      input.Path,
		AutoConfirm:     input.AutoConfirm,
		StoryResolution: input.StoryResolution,
		ContinueAfter:   input.ContinueAfter,
		Guidance:        input.Guidance,
	}
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID := rt.id
		ch, err := h.ImportFrom(context.Background(), opts)
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(bookID, "import", ev)
			}
			s.broadcastTyped(bookID, "import_done", nil)
		}()
		return map[string]any{"started": true}, nil
	})
}

func (s *server) engineSimulate(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID := rt.id
		ch, err := h.Simulate(context.Background())
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(bookID, "sim", ev)
			}
			s.broadcastTyped(bookID, "sim_done", nil)
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		bookID := rt.id
		ch, err := h.ImportSimulationProfile(context.Background(), strings.TrimSpace(input.Path))
		if err != nil {
			return nil, err
		}
		go func() {
			for ev := range ch {
				s.broadcastTyped(bookID, "sim", ev)
			}
			s.broadcastTyped(bookID, "sim_done", nil)
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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

func (s *server) engineDiag(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		path, err := diag.Export(store.NewStore(h.Dir()))
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": path}, nil
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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
			s.broadcastTyped(rt.id, "cocreate_delta", map[string]any{"kind": kind, "text": text})
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
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
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

func (s *server) cocreateCancel(w http.ResponseWriter, _ *http.Request) {
	s.withEngine(w, func(rt *bookRuntime, h *host.Host) (any, error) {
		rt.coMu.Lock()
		rt.cocreate = nil
		rt.coMu.Unlock()
		h.CancelCoCreate()
		return map[string]any{"cancelled": true}, nil
	})
}

func (s *server) chapters(w http.ResponseWriter, _ *http.Request) {
	s.bookMu.Lock()
	root := filepath.Join("output", "novel", "chapters")
	if rt := s.books[s.active]; rt != nil && rt.engine != nil {
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
func (s *server) config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.configGet(w, r)
	case http.MethodPost, http.MethodPut:
		s.configSave(w, r)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *server) configGet(w http.ResponseWriter, _ *http.Request) {
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		cfg = bootstrap.Config{}
	}
	cfg.FillDefaults()
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "openrouter"
	}
	pc := bootstrap.ProviderConfig{}
	if v, ok := cfg.Providers[provider]; ok {
		pc = v
	}
	providerConfigs := make(map[string]any, len(cfg.Providers))
	for name, configured := range cfg.Providers {
		providerConfigs[name] = map[string]any{
			"type":           configured.Type,
			"base_url":       configured.BaseURL,
			"models":         modelNames(configured.Models),
			"api_key_set":    configured.APIKey != "",
			"api_key_masked": maskAPIKey(configured.APIKey),
		}
	}
	writeJSON(w, map[string]any{
		"provider":         provider,
		"model":            cfg.ModelName,
		"models":           modelNames(pc.Models),
		"provider_configs": providerConfigs,
		"base_url":         pc.BaseURL,
		"api_key_set":      pc.APIKey != "",
		"api_key_masked":   maskAPIKey(pc.APIKey),
		"style":            cfg.Style,
		"path":             bootstrap.EffectiveConfigPath(),
	})
}

func (s *server) configSave(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string   `json:"provider"`
		APIKey   string   `json:"api_key"`
		BaseURL  string   `json:"base_url"`
		Model    string   `json:"model"`
		Models   []string `json:"models"`
		Type     string   `json:"type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid json: "+err.Error(), 400)
		return
	}

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		cfg = bootstrap.Config{}
	}

	provider := strings.TrimSpace(input.Provider)
	if provider == "" {
		provider = strings.TrimSpace(cfg.Provider)
	}
	if provider == "" {
		http.Error(w, "provider is required", 400)
		return
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = strings.TrimSpace(cfg.ModelName)
	}

	cfg.Provider = provider
	if model != "" {
		cfg.ModelName = model
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]bootstrap.ProviderConfig)
	}
	pc := cfg.Providers[provider]
	if key := strings.TrimSpace(input.APIKey); key != "" {
		pc.APIKey = key
	}
	if base := strings.TrimSpace(input.BaseURL); base != "" {
		pc.BaseURL = base
	}
	if typ := strings.TrimSpace(input.Type); typ != "" {
		pc.Type = typ
	}
	if len(pc.Models) == 0 && cfg.ModelName != "" {
		pc.Models = []bootstrap.ModelConfig{{Name: cfg.ModelName}}
	}
	if input.Models != nil {
		pc.Models = make([]bootstrap.ModelConfig, 0, len(input.Models))
		seen := map[string]bool{}
		for _, name := range input.Models {
			name = strings.TrimSpace(name)
			if name != "" && !seen[name] {
				pc.Models = append(pc.Models, bootstrap.ModelConfig{Name: name})
				seen[name] = true
			}
		}
		if len(pc.Models) == 0 && model != "" {
			pc.Models = []bootstrap.ModelConfig{{Name: model}}
		}
	}
	cfg.Providers[provider] = pc
	if cfg.Style == "" {
		cfg.Style = "default"
	}
	if cfg.Roles == nil {
		cfg.Roles = make(map[string]bootstrap.RoleConfig)
	}

	path := bootstrap.EffectiveConfigPath()
	if path == "" {
		http.Error(w, "cannot resolve config path", 500)
		return
	}
	if err := bootstrap.SaveConfig(path, cfg); err != nil {
		http.Error(w, "save config: "+err.Error(), 500)
		return
	}

	engineError := ""
	engineReady := false
	if err := s.reloadAllBooks(); err != nil {
		engineError = err.Error()
	} else {
		engineReady = true
	}
	writeJSON(w, map[string]any{
		"ok": true, "path": path, "provider": provider, "model": cfg.ModelName,
		"engine_ready": engineReady, "engine_error": engineError,
	})
}

func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return "****"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func modelNames(models []bootstrap.ModelConfig) []string {
	result := make([]string, 0, len(models))
	for _, model := range models {
		if name := strings.TrimSpace(model.Name); name != "" {
			result = append(result, name)
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
