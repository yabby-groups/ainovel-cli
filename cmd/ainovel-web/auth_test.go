package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func testAuth(t *testing.T) *authService {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := authConfig{issuer: "https://myna.test", clientID: "client", apiBase: "https://api.test/v1", dbPath: t.TempDir() + "/auth.sqlite", dataDir: t.TempDir(), sessionKey: key, encryptionKey: key}
	a, err := newAuthService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.close() })
	return a
}

func TestCredentialAndSessionAreUserScoped(t *testing.T) {
	a := testAuth(t)
	ctx := context.Background()
	for _, u := range []webUser{{ID: "u1", Name: "One"}, {ID: "u2", Name: "Two"}} {
		sealed, err := a.seal([]byte(`{"api_key":"key-` + u.ID + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.ExecContext(ctx, `INSERT INTO users(id,name,credential,created_at,updated_at) VALUES(?,?,?,0,0)`, u.ID, u.Name, sealed); err != nil {
			t.Fatal(err)
		}
	}
	c, err := a.credential(ctx, "u1")
	if err != nil || c.APIKey != "key-u1" {
		t.Fatalf("credential = %#v, %v", c, err)
	}
	token, err := a.createSession(ctx, &webUser{ID: "u1", Name: "One"})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	u, err := a.currentUser(r)
	if err != nil || u.ID != "u1" {
		t.Fatalf("user = %#v, %v", u, err)
	}
}

func TestLoadAuthConfigRejectsInvalidSecrets(t *testing.T) {
	t.Setenv("AINOVEL_MYNA_ISSUER", "https://myna.test")
	t.Setenv("AINOVEL_MYNA_API_BASE_URL", "https://api.test/v1")
	t.Setenv("AINOVEL_MYNA_CLIENT_ID", "client")
	t.Setenv("AINOVEL_WEB_SESSION_SECRET", base64.RawURLEncoding.EncodeToString(make([]byte, 8)))
	t.Setenv("AINOVEL_WEB_ENCRYPTION_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if _, err := loadAuthConfig(); err == nil {
		t.Fatal("expected invalid session secret")
	}
}

func TestBooksAreScopedToTheirUser(t *testing.T) {
	a := testAuth(t)
	s := &server{auth: a, books: make(map[string]*bookRuntime), active: make(map[string]string)}
	if s.bookKey("u1", "draft") == s.bookKey("u2", "draft") {
		t.Fatal("book runtime keys must include the user")
	}
	userOneBook := s.bookDirForID("u1", "draft")
	userTwoBook := s.bookDirForID("u2", "draft")
	if userOneBook == userTwoBook {
		t.Fatal("book directories must include the user")
	}
	if err := os.MkdirAll(filepath.Dir(userOneBook), 0o700); err != nil {
		t.Fatal(err)
	}
	s.bookMu.Lock()
	gotOne := s.discoverBookIDsLocked("u1")
	gotTwo := s.discoverBookIDsLocked("u2")
	s.bookMu.Unlock()
	if !reflect.DeepEqual(gotOne, []string{defaultBookID, "draft"}) || !reflect.DeepEqual(gotTwo, []string{defaultBookID}) {
		t.Fatalf("book discovery is not user-scoped: u1=%v u2=%v", gotOne, gotTwo)
	}
}
