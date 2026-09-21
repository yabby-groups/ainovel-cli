package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const sessionCookie = "ainovel_session"

type authConfig struct {
	issuer, clientID, apiBase, dbPath, dataDir string
	sessionKey, encryptionKey                  []byte
	secureCookie                               bool
}

type credential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	APIKey       string `json:"api_key"`
	ExpiresAt    int64  `json:"expires_at"`
}

type authService struct {
	cfg  authConfig
	db   *sql.DB
	http *http.Client
	box  cipher.AEAD
}

type contextUserKey struct{}

type webUser struct {
	ID   string
	Name string
}

func loadAuthConfig() (authConfig, error) {
	need := func(name string) (string, error) {
		v := strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
		if v == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		if _, err := url.ParseRequestURI(v); err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return v, nil
	}
	issuer, err := need("AINOVEL_MYNA_ISSUER")
	if err != nil {
		return authConfig{}, err
	}
	apiBase, err := need("AINOVEL_MYNA_API_BASE_URL")
	if err != nil {
		return authConfig{}, err
	}
	clientID := strings.TrimSpace(os.Getenv("AINOVEL_MYNA_CLIENT_ID"))
	if clientID == "" {
		return authConfig{}, errors.New("AINOVEL_MYNA_CLIENT_ID is required")
	}
	decode := func(name string) ([]byte, error) {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
		b, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%s must be base64url: %w", name, err)
		}
		return b, nil
	}
	sessionKey, err := decode("AINOVEL_WEB_SESSION_SECRET")
	if err != nil || len(sessionKey) < 32 {
		return authConfig{}, errors.New("AINOVEL_WEB_SESSION_SECRET must be at least 32 base64url-decoded bytes")
	}
	encryptionKey, err := decode("AINOVEL_WEB_ENCRYPTION_KEY")
	if err != nil || len(encryptionKey) != 32 {
		return authConfig{}, errors.New("AINOVEL_WEB_ENCRYPTION_KEY must decode to exactly 32 bytes")
	}
	dataDir := strings.TrimSpace(os.Getenv("AINOVEL_WEB_DATA_DIR"))
	if dataDir == "" {
		dataDir = "data"
	}
	dbPath := strings.TrimSpace(os.Getenv("AINOVEL_WEB_DB"))
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "ainovel-web.sqlite")
	}
	return authConfig{issuer: issuer, clientID: clientID, apiBase: apiBase, dbPath: dbPath, dataDir: dataDir, sessionKey: sessionKey, encryptionKey: encryptionKey, secureCookie: os.Getenv("AINOVEL_WEB_INSECURE_COOKIE") != "1"}, nil
}

func newAuthService(cfg authConfig) (*authService, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.dbPath), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.dataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cfg.dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
		CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, name TEXT NOT NULL, credential BLOB NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, expires_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS device_attempts (id TEXT PRIMARY KEY, device_code BLOB NOT NULL, user_code TEXT NOT NULL, verification_uri TEXT NOT NULL, expires_at INTEGER NOT NULL, next_poll_at INTEGER NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '');
		CREATE TABLE IF NOT EXISTS preferences (user_id TEXT PRIMARY KEY, model TEXT NOT NULL, active_book TEXT NOT NULL DEFAULT 'default', updated_at INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS workspace_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);`); err != nil {
		db.Close()
		return nil, err
	}
	_, _ = db.Exec(`ALTER TABLE preferences ADD COLUMN active_book TEXT NOT NULL DEFAULT 'default'`)
	block, err := aes.NewCipher(cfg.encryptionKey)
	if err != nil {
		db.Close()
		return nil, err
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &authService{cfg: cfg, db: db, http: &http.Client{Timeout: 15 * time.Second}, box: box}, nil
}

func (a *authService) seal(v []byte) ([]byte, error) {
	n := make([]byte, a.box.NonceSize())
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return nil, err
	}
	return a.box.Seal(n, n, v, nil), nil
}
func (a *authService) open(v []byte) ([]byte, error) {
	if len(v) < a.box.NonceSize() {
		return nil, errors.New("encrypted value is invalid")
	}
	return a.box.Open(nil, v[:a.box.NonceSize()], v[a.box.NonceSize():], nil)
}
func randomID(bytes int) (string, error) {
	b := make([]byte, bytes)
	_, err := io.ReadFull(rand.Reader, b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func (a *authService) endpoint(path string) string { return a.cfg.issuer + path }

func (a *authService) form(ctx context.Context, endpoint string, values url.Values, out any) (int, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (a *authService) startDevice(ctx context.Context) (map[string]any, error) {
	var result struct {
		DeviceCode, UserCode, VerificationURI, VerificationURIComplete string `json:"-"`
		ExpiresIn, Interval                                            int    `json:"-"`
	}
	var raw map[string]any
	status, err := a.form(ctx, a.endpoint("/oauth/device/code"), url.Values{"client_id": {a.cfg.clientID}, "scope": {"profile:read token_base:read token_base:write offline_access"}}, &raw)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, fmt.Errorf("Myna device authorization failed: %v", raw["error"])
	}
	result.DeviceCode, _ = raw["device_code"].(string)
	result.UserCode, _ = raw["user_code"].(string)
	result.VerificationURI, _ = raw["verification_uri"].(string)
	result.VerificationURIComplete, _ = raw["verification_uri_complete"].(string)
	result.ExpiresIn = int(number(raw["expires_in"]))
	result.Interval = int(number(raw["interval"]))
	if result.DeviceCode == "" || result.UserCode == "" || result.VerificationURI == "" {
		return nil, errors.New("Myna returned an incomplete device authorization")
	}
	if result.Interval < 1 {
		result.Interval = 3
	}
	if result.ExpiresIn < 1 {
		result.ExpiresIn = 600
	}
	id, err := randomID(24)
	if err != nil {
		return nil, err
	}
	sealed, err := a.seal([]byte(result.DeviceCode))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_, err = a.db.ExecContext(ctx, `INSERT INTO device_attempts(id,device_code,user_code,verification_uri,expires_at,next_poll_at,status) VALUES(?,?,?,?,?,?, 'pending')`, id, sealed, result.UserCode, result.VerificationURI, now.Add(time.Duration(result.ExpiresIn)*time.Second).Unix(), now.Add(time.Duration(result.Interval)*time.Second).Unix())
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "user_code": result.UserCode, "verification_uri": result.VerificationURI, "verification_uri_complete": result.VerificationURIComplete, "expires_in": result.ExpiresIn, "interval": result.Interval}, nil
}

func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case json.Number:
		x, _ := n.Int64()
		return x
	}
	return 0
}

func (a *authService) pollDevice(ctx context.Context, id string) (*webUser, error) {
	var sealed []byte
	var expires, next int64
	var state, previousErr string
	err := a.db.QueryRowContext(ctx, `SELECT device_code,expires_at,next_poll_at,status,error FROM device_attempts WHERE id=?`, id).Scan(&sealed, &expires, &next, &state, &previousErr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("authorization attempt not found")
	}
	if err != nil {
		return nil, err
	}
	if state == "approved" {
		return nil, errors.New("authorization has already been completed")
	}
	if state != "pending" {
		return nil, errors.New(previousErr)
	}
	now := time.Now().Unix()
	if now >= expires {
		_, _ = a.db.ExecContext(ctx, `UPDATE device_attempts SET status='expired',error='authorization expired' WHERE id=?`, id)
		return nil, errors.New("authorization expired")
	}
	if now < next {
		return nil, fmt.Errorf("authorization_pending")
	}
	deviceCode, err := a.open(sealed)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	status, err := a.form(ctx, a.endpoint("/oauth/token"), url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {a.cfg.clientID}, "device_code": {string(deviceCode)}}, &raw)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		kind, _ := raw["error"].(string)
		delay := int64(3)
		if kind == "slow_down" {
			delay = 8
		}
		if kind == "authorization_pending" || kind == "slow_down" {
			_, _ = a.db.ExecContext(ctx, `UPDATE device_attempts SET next_poll_at=? WHERE id=?`, now+delay, id)
			return nil, fmt.Errorf("%s", kind)
		}
		_, _ = a.db.ExecContext(ctx, `UPDATE device_attempts SET status='failed',error=? WHERE id=?`, kind, id)
		return nil, fmt.Errorf("Myna authorization failed: %s", kind)
	}
	access, _ := raw["access_token"].(string)
	refresh, _ := raw["refresh_token"].(string)
	if access == "" || refresh == "" {
		return nil, errors.New("Myna did not return offline credentials")
	}
	user, err := a.provision(ctx, access, refresh, now+number(raw["expires_in"]))
	if err != nil {
		return nil, err
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE device_attempts SET status='approved' WHERE id=?`, id)
	return user, nil
}

func (a *authService) jsonRequest(ctx context.Context, method, endpoint, token string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = strings.NewReader(string(b))
	}
	r, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(r)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (a *authService) provision(ctx context.Context, access, refresh string, expiresAt int64) (*webUser, error) {
	var me map[string]any
	status, err := a.jsonRequest(ctx, http.MethodGet, a.endpoint("/api/user/me/"), access, nil, &me)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, errors.New("could not read Myna profile")
	}
	profile, ok := me["user"].(map[string]any)
	if !ok {
		return nil, errors.New("Myna profile response is invalid")
	}
	id := fmt.Sprint(profile["id"])
	name := strings.TrimSpace(fmt.Sprint(profile["name"]))
	if id == "" || id == "<nil>" {
		return nil, errors.New("Myna profile has no id")
	}
	var listed map[string]any
	status, err = a.jsonRequest(ctx, http.MethodGet, a.endpoint("/api/token_base/token/my/list/?offset=0&size=20"), access, nil, &listed)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, errors.New("could not list Myna Token Base keys")
	}
	key := usableTokenKey(listed)
	if key == "" {
		requestID, err := randomID(18)
		if err != nil {
			return nil, err
		}
		var created map[string]any
		status, err = a.jsonRequest(ctx, http.MethodPost, a.endpoint("/api/token_base/token/create/"), access, map[string]any{"name": "ainovel-web", "request_id": "ainovel-" + requestID, "expired_at": -1, "unlimited_quota": true}, &created)
		if err != nil {
			return nil, err
		}
		if status/100 != 2 {
			return nil, errors.New("could not create Myna Token Base key")
		}
		key = nestedTokenKey(created)
		if key == "" {
			return nil, errors.New("Myna created a key but did not return its value")
		}
	}
	credJSON, err := json.Marshal(credential{AccessToken: access, RefreshToken: refresh, APIKey: key, ExpiresAt: expiresAt})
	if err != nil {
		return nil, err
	}
	sealed, err := a.seal(credJSON)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	_, err = a.db.ExecContext(ctx, `INSERT INTO users(id,name,credential,created_at,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,credential=excluded.credential,updated_at=excluded.updated_at`, id, name, sealed, now, now)
	if err != nil {
		return nil, err
	}
	_, _ = a.db.ExecContext(ctx, `INSERT INTO preferences(user_id,model,updated_at) VALUES(?,?,?) ON CONFLICT(user_id) DO NOTHING`, id, "gpt-5.6-luna", now)
	return &webUser{ID: id, Name: name}, nil
}

func usableTokenKey(response map[string]any) string {
	tokens, _ := response["tokens"].([]any)
	for _, item := range tokens {
		if row, ok := item.(map[string]any); ok {
			if key, _ := row["token_key"].(string); strings.TrimSpace(key) != "" {
				return strings.TrimSpace(key)
			}
		}
	}
	return ""
}
func nestedTokenKey(response map[string]any) string {
	token, _ := response["token"].(map[string]any)
	if token == nil {
		if order, ok := response["order"].(map[string]any); ok {
			token, _ = order["token"].(map[string]any)
		}
	}
	if token == nil {
		return ""
	}
	key, _ := token["token_key"].(string)
	return strings.TrimSpace(key)
}

func (a *authService) createSession(ctx context.Context, user *webUser) (string, error) {
	token, err := randomID(32)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.cfg.sessionKey)
	_, _ = mac.Write([]byte(token))
	sum := hex.EncodeToString(mac.Sum(nil))
	_, err = a.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, sum, user.ID, time.Now().Add(30*24*time.Hour).Unix())
	return token, err
}
func (a *authService) currentUser(r *http.Request) (*webUser, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, errors.New("authentication required")
	}
	mac := hmac.New(sha256.New, a.cfg.sessionKey)
	_, _ = mac.Write([]byte(cookie.Value))
	sum := hex.EncodeToString(mac.Sum(nil))
	var u webUser
	var expires int64
	err = a.db.QueryRow(`SELECT u.id,u.name,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=?`, sum).Scan(&u.ID, &u.Name, &expires)
	if err != nil || expires < time.Now().Unix() {
		return nil, errors.New("authentication required")
	}
	return &u, nil
}
func (a *authService) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.currentUser(r)
		if err != nil {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextUserKey{}, u)))
	})
}
func userFrom(r *http.Request) (*webUser, error) {
	u, ok := r.Context().Value(contextUserKey{}).(*webUser)
	if !ok || u == nil {
		return nil, errors.New("authentication required")
	}
	return u, nil
}
func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *authService) credential(ctx context.Context, userID string) (credential, error) {
	var sealed []byte
	if err := a.db.QueryRowContext(ctx, `SELECT credential FROM users WHERE id=?`, userID).Scan(&sealed); err != nil {
		return credential{}, err
	}
	raw, err := a.open(sealed)
	if err != nil {
		return credential{}, err
	}
	var c credential
	err = json.Unmarshal(raw, &c)
	return c, err
}

func (a *authService) models(ctx context.Context, userID string) ([]string, error) {
	c, err := a.credential(ctx, userID)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	status, err := a.jsonRequest(ctx, http.MethodGet, strings.TrimRight(a.cfg.apiBase, "/")+"/models", c.APIKey, nil, &raw)
	if err != nil {
		return nil, err
	}
	if status/100 != 2 {
		return nil, errors.New("could not load Myna model list")
	}
	models := make([]string, 0, len(raw.Data))
	for _, model := range raw.Data {
		if name := strings.TrimSpace(model.ID); name != "" {
			models = append(models, name)
		}
	}
	if len(models) == 0 {
		return nil, errors.New("Myna model list is empty")
	}
	return models, nil
}

func (a *authService) logout(ctx context.Context, r *http.Request) {
	u, err := a.currentUser(r)
	if err == nil {
		if c, e := a.credential(ctx, u.ID); e == nil && c.RefreshToken != "" {
			var ignored map[string]any
			_, _ = a.form(ctx, a.endpoint("/oauth/revoke"), url.Values{"client_id": {a.cfg.clientID}, "token": {c.RefreshToken}}, &ignored)
		}
		_, _ = a.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, u.ID)
		_, _ = a.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, u.ID)
	}
}
func (a *authService) close() error { return a.db.Close() }
