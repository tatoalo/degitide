package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tatoalo/degitide/internal/browser"
	"github.com/tatoalo/degitide/internal/db"
)

type fakeScheduler struct {
	running  bool
	triggers int
}

func (f *fakeScheduler) TriggerNow() error {
	f.triggers++
	f.running = true
	return nil
}

func (f *fakeScheduler) IsRunning() bool { return f.running }

func (f *fakeScheduler) LastError() string { return "" }

func TestDashboardAndAPIRoutes(t *testing.T) {
	var reauthenticationRequest map[string]string
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/check-session":
			_ = json.NewEncoder(w).Encode(map[string]bool{"valid": true})
		case "/tasks/reauthenticate":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&reauthenticationRequest); err != nil {
				t.Fatalf("decode reauthentication: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"authenticated": true, "session_saved": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer workerServer.Close()

	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()
	if err := database.SetMeta("last_pull", "2026-08-25T10:00:00Z"); err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	if err := database.Model(&db.Account{}).Where("id = ?", db.DefaultAccountID).Updates(map[string]any{"connected": true, "last_pull": "2026-08-25T10:00:00Z"}).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := &fakeScheduler{}
	service := New(database, browser.NewClient(workerServer.URL), scheduler)
	app := httptest.NewServer(service.Router())
	defer app.Close()

	resp, err := http.Get(app.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "degitide") {
		t.Fatalf("dashboard status = %d, body = %q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Connect DEGIRO") || !strings.Contains(string(body), "/api/degiro/reauthenticate") {
		t.Fatal("dashboard does not expose secure DEGIRO reauthentication")
	}
	if strings.Contains(string(body), "Open login") || strings.Contains(string(body), "/api/login/") {
		t.Fatal("dashboard still exposes the removed interactive login flow")
	}

	resp, err = http.Get(app.URL + "/api/status")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	_ = resp.Body.Close()
	if status["session_valid"] != true || status["last_pull"] != "2026-08-25T10:00:00Z" {
		t.Fatalf("unexpected status: %#v", status)
	}

	request, err := http.NewRequest(
		http.MethodPost,
		app.URL+"/api/degiro/reauthenticate",
		strings.NewReader(`{"username":"account-user","password":"account-password","totp":"123456"}`),
	)
	if err != nil {
		t.Fatalf("create reauthentication request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", app.URL)
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("reauthenticate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" ||
		reauthenticationRequest["username"] != "account-user" ||
		reauthenticationRequest["password"] != "account-password" ||
		reauthenticationRequest["totp"] != "123456" {
		t.Fatalf("reauthentication status = %d, request = %#v", resp.StatusCode, reauthenticationRequest)
	}
	request, _ = http.NewRequest(
		http.MethodPost,
		app.URL+"/api/degiro/reauthenticate",
		strings.NewReader(`{"username":"attacker","password":"not-forwarded","totp":"123456"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("cross-origin reauthentication: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || reauthenticationRequest["username"] != "account-user" {
		t.Fatalf("cross-origin reauthentication status = %d, worker request = %#v", resp.StatusCode, reauthenticationRequest)
	}

	service.loginWork <- struct{}{}
	request, _ = http.NewRequest(http.MethodPost, app.URL+"/api/pull/trigger", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", app.URL)
	resp, err = http.DefaultClient.Do(request)
	<-service.loginWork
	if err != nil {
		t.Fatalf("pull during reauthentication: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || scheduler.triggers != 0 {
		t.Fatalf("pull-during-login status = %d, calls = %d", resp.StatusCode, scheduler.triggers)
	}

	request, err = http.NewRequest(http.MethodPost, app.URL+"/api/pull/trigger", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("create trigger request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", app.URL)
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("trigger pull: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || scheduler.triggers != 1 {
		t.Fatalf("trigger status = %d, calls = %d", resp.StatusCode, scheduler.triggers)
	}

	request, err = http.NewRequest(http.MethodPost, app.URL+"/api/login/start", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("create removed-login request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", app.URL)
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("call removed login endpoint: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("removed login endpoint status = %d", resp.StatusCode)
	}
}

func TestRemoteDEGIROReauthenticationRequiresHTTPSAndStrictJSON(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	workerCalled := make(chan struct{}, 1)
	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case workerCalled <- struct{}{}:
		default:
		}
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer workerServer.Close()
	handler := New(
		database,
		browser.NewClient(workerServer.URL),
		&fakeScheduler{},
		Options{AllowedHosts: "dg.example"},
	).Router()

	request := httptest.NewRequest(
		http.MethodPost,
		"http://dg.example/api/degiro/reauthenticate",
		strings.NewReader(`{"username":"owner","password":"secret","totp":"123456"}`),
	)
	request.Host = "dg.example"
	request.Header.Set("Origin", "http://dg.example")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext remote reauthentication status = %d", recorder.Code)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"http://dg.example/api/degiro/reauthenticate",
		strings.NewReader(`{"username":"owner","password":"secret","totp":"123456","persist":true}`),
	)
	request.Host = "dg.example"
	request.Header.Set("Origin", "https://dg.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown-field reauthentication status = %d", recorder.Code)
	}
	select {
	case <-workerCalled:
		t.Fatal("worker received an invalid credential request")
	default:
	}
}

func TestDashboardAdminSetupLoginAndGuards(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	workerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"valid": false})
	}))
	defer workerServer.Close()

	app := httptest.NewServer(New(
		database,
		browser.NewClient(workerServer.URL),
		&fakeScheduler{},
		Options{APIToken: "internal-session-key", AllowedHosts: "127.0.0.1"},
	).Router())
	defer app.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(app.URL + "/")
	if err != nil {
		t.Fatalf("uninitialized dashboard: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("uninitialized dashboard status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if challenge := resp.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("unexpected browser auth challenge: %q", challenge)
	}

	resp, err = client.Get(app.URL + "/setup")
	if err != nil {
		t.Fatalf("get setup page: %v", err)
	}
	setupBody, _ := io.ReadAll(resp.Body)
	setupCookies := resp.Cookies()
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(setupBody), "Create your workspace") ||
		!strings.Contains(string(setupBody), "separate from your DEGIRO") ||
		!strings.Contains(string(setupBody), `name="csrf_token"`) ||
		strings.Contains(string(setupBody), authCSRFPlaceholder) ||
		resp.Header.Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("unexpected setup page status = %d, body = %q", resp.StatusCode, setupBody)
	}
	if len(setupCookies) != 1 || setupCookies[0].Name != localAuthCSRFCookie ||
		!setupCookies[0].HttpOnly || setupCookies[0].SameSite != http.SameSiteStrictMode ||
		setupCookies[0].MaxAge != 0 || strings.Contains(resp.Header.Get("Set-Cookie"), "Max-Age") {
		t.Fatalf("unexpected setup CSRF cookie: %#v", setupCookies)
	}
	for _, asset := range []string{"/auth.css", "/auth.js"} {
		resp, err = client.Get(app.URL + asset)
		if err != nil {
			t.Fatalf("get public auth asset %s: %v", asset, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("auth asset %s status = %d, cache-control = %q", asset, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}

	resp, err = client.Get(app.URL + "/api/status")
	if err != nil {
		t.Fatalf("uninitialized API: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatalf("uninitialized API status = %d, challenge = %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	request, _ := http.NewRequest(http.MethodGet, app.URL+"/setup", nil)
	request.Host = "evil.example"
	resp, err = client.Do(request)
	if err != nil {
		t.Fatalf("bad-host setup request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad-host setup status = %d", resp.StatusCode)
	}

	request, _ = http.NewRequest(http.MethodPost, app.URL+"/setup", strings.NewReader(url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", app.URL)
	resp, err = client.Do(request)
	if err != nil {
		t.Fatalf("submit setup without CSRF token: %v", err)
	}
	missingTokenBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(missingTokenBody), `data-error="csrf_missing"`) ||
		!strings.Contains(string(missingTokenBody), `value="owner"`) {
		t.Fatalf("missing CSRF token status = %d, body = %q", resp.StatusCode, missingTokenBody)
	}

	resp = postForm(t, client, app.URL+"/setup", "https://evil.example", url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup?error=origin" {
		t.Fatalf("cross-origin setup status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp = postForm(t, client, app.URL+"/setup", "", url.Values{
		"username":         {"bad space"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	invalidUsernameBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(invalidUsernameBody), `data-error="username"`) ||
		!strings.Contains(string(invalidUsernameBody), `value="bad space"`) {
		t.Fatalf("invalid username status = %d, body = %q", resp.StatusCode, invalidUsernameBody)
	}
	_ = resp.Body.Close()

	resp = postForm(t, client, app.URL+"/setup", app.URL, url.Values{
		"username":         {"owner"},
		"password":         {"too-short"},
		"password_confirm": {"too-short"},
	})
	shortPasswordBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(shortPasswordBody), `data-error="password"`) ||
		!strings.Contains(string(shortPasswordBody), `value="owner"`) {
		t.Fatalf("short password status = %d, body = %q", resp.StatusCode, shortPasswordBody)
	}
	_ = resp.Body.Close()

	resp = postForm(t, client, app.URL+"/setup", app.URL, url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery stapler"},
	})
	passwordMismatchBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(passwordMismatchBody), `data-error="mismatch"`) ||
		!strings.Contains(string(passwordMismatchBody), `value="owner"`) {
		t.Fatalf("password mismatch status = %d, body = %q", resp.StatusCode, passwordMismatchBody)
	}
	_ = resp.Body.Close()

	resp = postForm(t, client, app.URL+"/setup", app.URL, url.Values{
		"username":         {"Owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	_ = resp.Body.Close()
	sessionCookie := responseCookie(cookies, dashboardSessionCookie)
	csrfCookie := responseCookie(cookies, localAuthCSRFCookie)
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode ||
		csrfCookie == nil || csrfCookie.MaxAge >= 0 {
		t.Fatalf("unexpected setup cookie: %#v", cookies)
	}
	admin, err := database.GetAdmin()
	if err != nil || admin == nil {
		t.Fatalf("stored admin = %#v, err = %v", admin, err)
	}
	if admin.Username != "owner" || admin.PasswordHash == "correct horse battery staple" || !verifyPassword(admin.PasswordHash, "correct horse battery staple") {
		t.Fatalf("unexpected stored admin: %#v", admin)
	}

	resp, err = client.Get(app.URL + "/")
	if err != nil {
		t.Fatalf("setup-authenticated dashboard: %v", err)
	}
	dashboardBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(dashboardBody), "Sign out") {
		t.Fatalf("authenticated dashboard status = %d", resp.StatusCode)
	}
	resp, err = client.Get(app.URL + "/api/status")
	if err != nil {
		t.Fatalf("cookie-authenticated API: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authenticated API status = %d", resp.StatusCode)
	}

	resp, err = client.Get(app.URL + "/setup")
	if err != nil {
		t.Fatalf("get locked setup: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("locked setup status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp = postForm(t, client, app.URL+"/setup", app.URL, url.Values{
		"username":         {"attacker"},
		"password":         {"different secure password"},
		"password_confirm": {"different secure password"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("second setup status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	admin, _ = database.GetAdmin()
	if admin == nil || admin.Username != "owner" {
		t.Fatalf("administrator was replaced: %#v", admin)
	}

	request, _ = http.NewRequest(http.MethodPost, app.URL+"/logout", nil)
	request.Header.Set("Origin", app.URL)
	resp, err = client.Do(request)
	if err != nil {
		t.Fatalf("sign out: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	request, _ = http.NewRequest(
		http.MethodPost,
		app.URL+"/api/degiro/reauthenticate",
		strings.NewReader(`{"username":"account","password":"secret","totp":"123456"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", app.URL)
	resp, err = client.Do(request)
	if err != nil {
		t.Fatalf("signed-out reauthentication: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed-out reauthentication status = %d", resp.StatusCode)
	}

	resp, err = client.Get(app.URL + "/")
	if err != nil {
		t.Fatalf("dashboard after logout: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("post-logout status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = client.Get(app.URL + "/login")
	if err != nil {
		t.Fatalf("get login page: %v", err)
	}
	loginBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(loginBody), `autocomplete="username"`) ||
		!strings.Contains(string(loginBody), `name="csrf_token"`) ||
		strings.Contains(string(loginBody), authCSRFPlaceholder) ||
		strings.Contains(string(loginBody), "docker-dashboard-token") {
		t.Fatalf("unexpected login page status = %d, body = %q", resp.StatusCode, loginBody)
	}

	resp = postForm(t, client, app.URL+"/login", app.URL, url.Values{
		"username": {"owner"},
		"password": {"wrong password"},
	})
	badLoginBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(badLoginBody), `data-error="invalid"`) ||
		!strings.Contains(string(badLoginBody), `value="owner"`) {
		t.Fatalf("bad login status = %d, body = %q", resp.StatusCode, badLoginBody)
	}
	resp = postForm(t, client, app.URL+"/login", "", url.Values{
		"username": {"OWNER"},
		"password": {"correct horse battery staple"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login status = %d, location = %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	request, _ = http.NewRequest(http.MethodGet, app.URL+"/api/status", nil)
	request.SetBasicAuth("degitide", "internal-session-key")
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("basic-auth API request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("basic-auth API status = %d", resp.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, app.URL+"/api/pull/trigger", strings.NewReader("{}"))
	request.SetBasicAuth("degitide", "internal-session-key")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("cross-origin request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", resp.StatusCode)
	}
}

func TestDashboardSetupMarksHTTPSCookieSecure(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	handler := New(database, browser.NewClient("http://worker.invalid"), &fakeScheduler{}, Options{
		APIToken:     "internal-session-key",
		AllowedHosts: "127.0.0.1",
	}).Router()
	form := url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	}
	csrfToken, err := newAuthCSRFToken()
	if err != nil {
		t.Fatalf("create CSRF token: %v", err)
	}
	form.Set(authCSRFField, csrfToken)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://127.0.0.1")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.AddCookie(&http.Cookie{Name: secureAuthCSRFCookie, Value: csrfToken})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	response := recorder.Result()
	defer response.Body.Close()
	cookies := response.Cookies()
	sessionCookie := responseCookie(cookies, dashboardSessionCookie)
	csrfCookie := responseCookie(cookies, secureAuthCSRFCookie)
	if response.StatusCode != http.StatusSeeOther || sessionCookie == nil || !sessionCookie.Secure ||
		csrfCookie == nil || !csrfCookie.Secure || csrfCookie.MaxAge >= 0 {
		t.Fatalf("secure setup response = %d, cookies = %#v", response.StatusCode, cookies)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://127.0.0.1")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.AddCookie(&http.Cookie{Name: secureAuthCSRFCookie, Value: csrfToken})
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup?error=origin" {
		t.Fatalf("cross-scheme setup response = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestAuthCSRFTokenSurvivesConcurrentPageLoads(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	app := httptest.NewServer(New(
		database,
		browser.NewClient("http://worker.invalid"),
		&fakeScheduler{},
		Options{APIToken: "internal-session-key", AllowedHosts: "127.0.0.1"},
	).Router())
	defer app.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	first, err := client.Get(app.URL + "/setup")
	if err != nil {
		t.Fatalf("get first setup form: %v", err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	_ = first.Body.Close()
	firstToken := authTokenFromBody(t, firstBody)
	if len(first.Cookies()) != 1 || first.Cookies()[0].MaxAge != 0 ||
		strings.Contains(first.Header.Get("Set-Cookie"), "Max-Age") {
		t.Fatalf("first form did not create a session CSRF cookie: %#v", first.Cookies())
	}

	second, err := client.Get(app.URL + "/setup")
	if err != nil {
		t.Fatalf("get second setup form: %v", err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	_ = second.Body.Close()
	if secondToken := authTokenFromBody(t, secondBody); secondToken != firstToken {
		t.Fatalf("second form rotated the CSRF token: first %q second %q", firstToken, secondToken)
	}
	secondCookie := responseCookie(second.Cookies(), localAuthCSRFCookie)
	if secondCookie == nil || secondCookie.Value != firstToken || secondCookie.MaxAge != 0 ||
		strings.Contains(second.Header.Get("Set-Cookie"), "Max-Age") {
		t.Fatalf("second form did not preserve the token in a session cookie: %#v", second.Cookies())
	}

	response := postForm(t, client, app.URL+"/setup", app.URL, url.Values{
		authCSRFField:      {firstToken},
		"username":         {"bad space"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-error="username"`) {
		t.Fatalf("first form stopped working after second load: status = %d, body = %q", response.StatusCode, body)
	}
}

func TestAuthCSRFFailuresAreDiscriminatedAndLogged(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	handler := New(database, browser.NewClient("http://worker.invalid"), &fakeScheduler{}, Options{
		APIToken:     "internal-session-key",
		AllowedHosts: "127.0.0.1",
	}).Router()

	oldLogWriter := log.Writer()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(oldLogWriter)

	staleToken, err := newAuthCSRFToken()
	if err != nil {
		t.Fatalf("create stale token: %v", err)
	}
	response := submitAuthForm(handler, nil, "http://127.0.0.1/setup", "http://127.0.0.1", "", url.Values{
		authCSRFField:      {staleToken},
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	freshCookie := responseCookie(response.Cookies(), localAuthCSRFCookie)
	if response.StatusCode != http.StatusOK || freshCookie == nil ||
		!strings.Contains(string(body), `data-error="csrf_expired"`) ||
		authTokenFromBody(t, body) != freshCookie.Value ||
		!strings.Contains(string(body), `value="owner"`) {
		t.Fatalf("missing-cookie response = %d, cookies = %#v, body = %q", response.StatusCode, response.Cookies(), body)
	}

	response = submitAuthForm(handler, freshCookie, "http://127.0.0.1/setup", "http://127.0.0.1", "", url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-error="csrf_missing"`) ||
		authTokenFromBody(t, body) != freshCookie.Value {
		t.Fatalf("missing-token response = %d, body = %q", response.StatusCode, body)
	}

	mismatchedToken, err := newAuthCSRFToken()
	if err != nil {
		t.Fatalf("create mismatched token: %v", err)
	}
	response = submitAuthForm(handler, freshCookie, "http://127.0.0.1/setup", "http://127.0.0.1", "", url.Values{
		authCSRFField:      {mismatchedToken},
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-error="csrf_mismatch"`) ||
		authTokenFromBody(t, body) != freshCookie.Value {
		t.Fatalf("mismatched-token response = %d, body = %q", response.StatusCode, body)
	}

	response = submitAuthForm(handler, freshCookie, "http://127.0.0.1/setup", "https://evil.example", "", url.Values{
		authCSRFField:      {freshCookie.Value},
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/setup?error=origin" {
		t.Fatalf("cross-origin response = %d %q", response.StatusCode, response.Header.Get("Location"))
	}

	response = submitAuthForm(handler, freshCookie, "http://127.0.0.1/setup", "null", "same-origin", url.Values{
		authCSRFField:      {freshCookie.Value},
		"username":         {"bad space"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-error="username"`) {
		t.Fatalf("Firefox null-origin form did not reach the CSRF-protected handler: %d %q", response.StatusCode, body)
	}

	response = submitAuthForm(handler, freshCookie, "http://127.0.0.1/setup", "null", "same-origin", url.Values{
		"username":         {"owner"},
		"password":         {"correct horse battery staple"},
		"password_confirm": {"correct horse battery staple"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `data-error="csrf_missing"`) {
		t.Fatalf("Firefox null-origin form bypassed the CSRF token: %d %q", response.StatusCode, body)
	}

	for _, reason := range []string{
		authCSRFCookieMissing,
		authCSRFTokenMissing,
		authCSRFTokenMismatch,
		"origin_mismatch",
	} {
		if !strings.Contains(logs.String(), `reason="`+reason+`"`) {
			t.Fatalf("missing rejection reason %q in logs: %q", reason, logs.String())
		}
	}

	authScript, err := webFiles.ReadFile("static/auth.js")
	if err != nil {
		t.Fatalf("read auth script: %v", err)
	}
	for _, code := range []string{"csrf_expired", "csrf_missing", "csrf_mismatch", "origin"} {
		if !strings.Contains(string(authScript), code+":") {
			t.Fatalf("auth script has no message for %q", code)
		}
	}
}

func TestRemoteAdminSetupRequiresHTTPS(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "degitide.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	handler := New(database, browser.NewClient("http://worker.invalid"), &fakeScheduler{}, Options{
		APIToken:     "internal-session-key",
		AllowedHosts: "dashboard.example",
	}).Router()
	request := httptest.NewRequest(http.MethodGet, "http://dashboard.example/setup", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("remote HTTP setup status = %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "http://dashboard.example/setup", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("remote HTTPS setup status = %d", recorder.Code)
	}
}

func TestRequestHasSameOriginBehindReverseProxy(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		origin    string
		fetchSite string
		want      bool
	}{
		{name: "public HTTPS origin", host: "dashboard.example", origin: "https://dashboard.example", want: true},
		{name: "implicit and explicit HTTPS ports", host: "dashboard.example:443", origin: "https://dashboard.example", want: true},
		{name: "explicit and implicit HTTPS ports", host: "dashboard.example", origin: "https://dashboard.example:443", want: true},
		{name: "case insensitive host", host: "DASHBOARD.EXAMPLE", origin: "https://dashboard.example", want: true},
		{name: "missing origin with same-origin metadata", host: "dashboard.example", fetchSite: "same-origin", want: true},
		{name: "missing origin without metadata", host: "dashboard.example", want: false},
		{name: "missing origin with same-site metadata", host: "dashboard.example", fetchSite: "same-site", want: false},
		{name: "opaque origin with same-origin metadata", host: "dashboard.example", origin: "null", fetchSite: "same-origin", want: false},
		{name: "Authelia sibling origin", host: "dashboard.example", origin: "https://auth.example", fetchSite: "same-site", want: false},
		{name: "cross scheme", host: "dashboard.example", origin: "http://dashboard.example", want: false},
		{name: "non-default port", host: "dashboard.example", origin: "https://dashboard.example:8443", want: false},
		{name: "origin with path", host: "dashboard.example", origin: "https://dashboard.example/setup", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", nil)
			request.Host = test.host
			request.Header.Set("X-Forwarded-Proto", "https")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := requestHasSameOrigin(request); got != test.want {
				t.Fatalf("requestHasSameOrigin() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAuthFormCSRFOriginFallback(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		origin      string
		fetchSite   string
		want        bool
	}{
		{name: "missing metadata for proxy compatibility", want: true},
		{name: "missing origin with same-origin metadata", fetchSite: "same-origin", want: true},
		{name: "missing origin with same-site metadata", fetchSite: "same-site", want: false},
		{name: "missing origin with none metadata", fetchSite: "none", want: false},
		{name: "opaque origin with same-origin metadata", origin: "null", fetchSite: "same-origin", want: true},
		{name: "case-insensitive opaque origin", origin: "NULL", fetchSite: "same-origin", want: true},
		{name: "opaque origin without metadata", origin: "null", want: false},
		{name: "opaque origin with same-site metadata", origin: "null", fetchSite: "same-site", want: false},
		{name: "opaque origin with none metadata", origin: "null", fetchSite: "none", want: false},
		{name: "opaque origin with cross-site metadata", origin: "null", fetchSite: "cross-site", want: false},
		{name: "explicit origin uses canonical check", origin: "https://dashboard.example", fetchSite: "same-origin", want: false},
		{name: "API is never eligible", path: "/api/pull/trigger", origin: "null", fetchSite: "same-origin", want: false},
		{name: "JSON is never eligible", contentType: "application/json", origin: "null", fetchSite: "same-origin", want: false},
		{name: "GET is never eligible", method: http.MethodGet, origin: "null", fetchSite: "same-origin", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodPost
			}
			path := test.path
			if path == "" {
				path = "/setup"
			}
			contentType := test.contentType
			if contentType == "" {
				contentType = "application/x-www-form-urlencoded"
			}
			request := httptest.NewRequest(method, "http://dashboard.example"+path, nil)
			request.Header.Set("Content-Type", contentType)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if got := authFormMayUseCSRF(request); got != test.want {
				t.Fatalf("authFormMayUseCSRF() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOriginGuardRedirectsBrowserFormsButKeepsAPIError(t *testing.T) {
	handler := guardRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), "dashboard.example")

	request := httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "https://auth.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup?error=origin" {
		t.Fatalf("setup rejection = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/api/pull/trigger", strings.NewReader("{}"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "https://auth.example")
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "same-origin request required") {
		t.Fatalf("API rejection = %d %q", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/api/pull/trigger", strings.NewReader("{}"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "same-origin request required") {
		t.Fatalf("opaque-origin API rejection = %d %q", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", strings.NewReader("csrf_token=present"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Sec-Fetch-Site", "same-site")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup?error=origin" {
		t.Fatalf("same-site form rejection = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", strings.NewReader("csrf_token=present"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("metadata-free token-protected form was rejected before its handler: %d %q", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", strings.NewReader("csrf_token=present"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("Firefox null-origin form was rejected before its handler: %d %q", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", strings.NewReader("csrf_token=present"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-site")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup?error=origin" {
		t.Fatalf("untrusted null-origin form rejection = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodPost, "http://dashboard.example/setup", strings.NewReader("csrf_token=present"))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/setup?error=origin" {
		t.Fatalf("cross-site form rejection = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
}

func postForm(t *testing.T, client *http.Client, target, origin string, values url.Values) *http.Response {
	t.Helper()
	targetURL, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse form target: %v", err)
	}
	if client.Jar != nil {
		for _, cookie := range client.Jar.Cookies(targetURL) {
			if cookie.Name == localAuthCSRFCookie || cookie.Name == secureAuthCSRFCookie {
				values.Set(authCSRFField, cookie.Value)
				break
			}
		}
	}
	request, err := http.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("create form request: %v", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("submit form: %v", err)
	}
	return response
}

func submitAuthForm(handler http.Handler, cookie *http.Cookie, target, origin, fetchSite string, values url.Values) *http.Response {
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		request.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func authTokenFromBody(t *testing.T, body []byte) string {
	t.Helper()
	body = []byte(strings.Join(strings.Fields(string(body)), " "))
	const marker = `name="csrf_token" type="hidden" value="`
	start := strings.Index(string(body), marker)
	if start == -1 {
		t.Fatalf("authentication page has no CSRF field: %q", body)
	}
	start += len(marker)
	remaining := string(body[start:])
	end := strings.IndexByte(remaining, '"')
	if end == -1 {
		t.Fatalf("authentication page has an unterminated CSRF field: %q", body)
	}
	return remaining[:end]
}

func responseCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
