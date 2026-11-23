package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/tatoalo/degitide/internal/db"
)

const (
	dashboardSessionCookie  = "degitide_session"
	secureAuthCSRFCookie    = "__Host-degitide_csrf"
	localAuthCSRFCookie     = "degitide_csrf"
	authCSRFField           = "csrf_token"
	authCSRFPlaceholder     = "__DEGITIDE_CSRF_TOKEN__"
	authErrorPlaceholder    = "__DEGITIDE_ERROR_CODE__"
	authUsernamePlaceholder = "__DEGITIDE_USERNAME__"
	authCSRFTokenBytes      = 32
	minimumPasswordLength   = 12
	maximumPasswordLength   = 256
)

const (
	authCSRFCookieMissing = "csrf_cookie_missing"
	authCSRFCookieInvalid = "csrf_cookie_invalid"
	authCSRFTokenMissing  = "csrf_token_missing"
	authCSRFTokenInvalid  = "csrf_token_invalid"
	authCSRFTokenMismatch = "csrf_token_mismatch"
)

type authPageValues struct {
	errorCode string
	username  string
}

var adminUsernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._@+-]{2,63}$`)
var passwordWork = make(chan struct{}, 2)

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if s.options.APIToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !credentialTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, errors.New("HTTPS is required for remote administrator setup"))
		return
	}
	admin, err := s.db.GetAdmin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator state unavailable"))
		return
	}
	if admin != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	serveAuthPage(w, r, "static/setup.html", authPageValues{})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.options.APIToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !credentialTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, errors.New("HTTPS is required for remote administrator setup"))
		return
	}
	admin, err := s.db.GetAdmin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator state unavailable"))
		return
	}
	if admin != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		serveAuthError(w, r, "static/setup.html", "invalid", "form_parse_failed", "")
		return
	}
	usernameInput := authUsernameForRender(r.PostForm.Get("username"))
	if reason := authCSRFRejection(r); reason != "" {
		serveAuthError(w, r, "static/setup.html", authCSRFErrorCode(reason), reason, usernameInput)
		return
	}
	username, validUsername := normalizeAdminUsername(r.PostForm.Get("username"))
	if !validUsername {
		serveAuthError(w, r, "static/setup.html", "username", "username_invalid", usernameInput)
		return
	}
	password := r.PostForm.Get("password")
	if len(password) < minimumPasswordLength || len(password) > maximumPasswordLength {
		serveAuthError(w, r, "static/setup.html", "password", "password_length_invalid", usernameInput)
		return
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(r.PostForm.Get("password_confirm"))) != 1 {
		serveAuthError(w, r, "static/setup.html", "mismatch", "password_confirmation_mismatch", usernameInput)
		return
	}
	if !beginPasswordWork() {
		serveAuthError(w, r, "static/setup.html", "busy", "password_work_busy", usernameInput)
		return
	}
	passwordHash, err := hashPassword(password)
	endPasswordWork()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator setup failed"))
		return
	}
	admin, err = s.db.CreateAdmin(username, passwordHash)
	if errors.Is(err, db.ErrAdminAlreadyExists) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator setup failed"))
		return
	}
	clearAuthCSRFCookie(w, r)
	setDashboardSession(w, r, s.options.APIToken, admin)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.options.APIToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !credentialTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, errors.New("HTTPS is required for remote administrator login"))
		return
	}
	admin, err := s.db.GetAdmin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator state unavailable"))
		return
	}
	if admin == nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	serveAuthPage(w, r, "static/login.html", authPageValues{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.options.APIToken == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !credentialTransportAllowed(r) {
		writeError(w, http.StatusUpgradeRequired, errors.New("HTTPS is required for remote administrator login"))
		return
	}
	admin, err := s.db.GetAdmin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("administrator state unavailable"))
		return
	}
	if admin == nil {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	if err := r.ParseForm(); err != nil {
		serveAuthError(w, r, "static/login.html", "invalid", "form_parse_failed", "")
		return
	}
	usernameInput := authUsernameForRender(r.PostForm.Get("username"))
	if reason := authCSRFRejection(r); reason != "" {
		serveAuthError(w, r, "static/login.html", authCSRFErrorCode(reason), reason, usernameInput)
		return
	}
	username, _ := normalizeAdminUsername(r.PostForm.Get("username"))
	expectedUsername := sha256.Sum256([]byte(admin.Username))
	suppliedUsername := sha256.Sum256([]byte(username))
	usernameValid := subtle.ConstantTimeCompare(suppliedUsername[:], expectedUsername[:]) == 1
	if !beginPasswordWork() {
		serveAuthError(w, r, "static/login.html", "busy", "password_work_busy", usernameInput)
		return
	}
	passwordValid := verifyPassword(admin.PasswordHash, r.PostForm.Get("password"))
	endPasswordWork()
	if !usernameValid || !passwordValid {
		serveAuthError(w, r, "static/login.html", "invalid", "credentials_invalid", usernameInput)
		return
	}

	clearAuthCSRFCookie(w, r)
	setDashboardSession(w, r, s.options.APIToken, admin)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardSessionCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func authenticateDashboard(next http.Handler, database *db.DB, sessionKey string) http.Handler {
	if sessionKey == "" {
		return next
	}
	expectedBasicUser := sha256.Sum256([]byte("degitide"))
	expectedBasicToken := sha256.Sum256([]byte(sessionKey))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		admin, err := database.GetAdmin()
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("administrator state unavailable"))
			return
		}
		if admin == nil {
			if r.URL.Path == "/setup" || isPublicAuthAsset(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			unauthenticated(w, r, "/setup", http.StatusServiceUnavailable, "dashboard setup required")
			return
		}
		if r.URL.Path == "/login" || r.URL.Path == "/setup" || isPublicAuthAsset(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		username, password, basicOK := r.BasicAuth()
		basicUser := sha256.Sum256([]byte(username))
		basicToken := sha256.Sum256([]byte(password))
		basicValid := basicOK && subtle.ConstantTimeCompare(basicUser[:], expectedBasicUser[:]) == 1 &&
			subtle.ConstantTimeCompare(basicToken[:], expectedBasicToken[:]) == 1

		cookie, cookieErr := r.Cookie(dashboardSessionCookie)
		cookieValue := ""
		if cookieErr == nil {
			cookieValue = cookie.Value
		}
		expectedSession := sha256.Sum256([]byte(dashboardSessionValue(sessionKey, admin)))
		suppliedSession := sha256.Sum256([]byte(cookieValue))
		cookieValid := cookieErr == nil && subtle.ConstantTimeCompare(suppliedSession[:], expectedSession[:]) == 1

		if !basicValid && !cookieValid {
			unauthenticated(w, r, "/login", http.StatusUnauthorized, "dashboard authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isPublicAuthAsset(path string) bool {
	return path == "/auth.css" || path == "/auth.js" || path == "/base.css" || path == "/brand.svg" || path == "/fonts/manrope.woff2" || path == "/fonts/plex-mono.woff2"
}

func unauthenticated(w http.ResponseWriter, r *http.Request, redirectPath string, apiStatus int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") {
		http.Redirect(w, r, redirectPath, http.StatusSeeOther)
		return
	}
	writeError(w, apiStatus, errors.New(message))
}

func serveAuthPage(w http.ResponseWriter, r *http.Request, path string, values authPageValues) {
	content, err := webFiles.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("authentication page unavailable"))
		return
	}
	token, err := authCSRFTokenForRequest(r)
	if err != nil || !bytes.Contains(content, []byte(authCSRFPlaceholder)) ||
		!bytes.Contains(content, []byte(authErrorPlaceholder)) ||
		!bytes.Contains(content, []byte(authUsernamePlaceholder)) {
		writeError(w, http.StatusInternalServerError, errors.New("authentication page unavailable"))
		return
	}
	content = bytes.Replace(content, []byte(authCSRFPlaceholder), []byte(token), 1)
	content = bytes.Replace(content, []byte(authErrorPlaceholder), []byte(html.EscapeString(values.errorCode)), 1)
	content = bytes.Replace(content, []byte(authUsernamePlaceholder), []byte(html.EscapeString(values.username)), 1)
	// Reissue the same value as a session cookie so deployments immediately
	// replace the former ten-minute cookie without invalidating open forms.
	setAuthCSRFCookie(w, r, token)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func authCSRFTokenForRequest(r *http.Request) (string, error) {
	cookie, err := r.Cookie(authCSRFCookieName(r))
	if err == nil && validAuthCSRFToken(cookie.Value) {
		return cookie.Value, nil
	}
	return newAuthCSRFToken()
}

func newAuthCSRFToken() (string, error) {
	value := make([]byte, authCSRFTokenBytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func authCSRFRejection(r *http.Request) string {
	submitted := r.PostForm.Get(authCSRFField)
	cookie, err := r.Cookie(authCSRFCookieName(r))
	if err != nil {
		return authCSRFCookieMissing
	}
	if !validAuthCSRFToken(cookie.Value) {
		return authCSRFCookieInvalid
	}
	if submitted == "" {
		return authCSRFTokenMissing
	}
	if !validAuthCSRFToken(submitted) {
		return authCSRFTokenInvalid
	}
	expected := sha256.Sum256([]byte(cookie.Value))
	actual := sha256.Sum256([]byte(submitted))
	if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
		return authCSRFTokenMismatch
	}
	return ""
}

func validAuthCSRFToken(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(authCSRFTokenBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == authCSRFTokenBytes
}

func authCSRFErrorCode(reason string) string {
	switch reason {
	case authCSRFCookieMissing, authCSRFCookieInvalid:
		return "csrf_expired"
	case authCSRFTokenMissing, authCSRFTokenInvalid:
		return "csrf_missing"
	default:
		return "csrf_mismatch"
	}
}

func authCSRFCookieName(r *http.Request) string {
	if requestIsSecure(r) {
		return secureAuthCSRFCookie
	}
	return localAuthCSRFCookie
}

func setAuthCSRFCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCSRFCookieName(r),
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearAuthCSRFCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCSRFCookieName(r),
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func normalizeAdminUsername(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	return value, adminUsernamePattern.MatchString(value)
}

func credentialTransportAllowed(r *http.Request) bool {
	host := normalizeHostname(r.Host)
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return requestIsSecure(r) || (ip != nil && ip.IsLoopback())
}

func beginPasswordWork() bool {
	select {
	case passwordWork <- struct{}{}:
		return true
	default:
		return false
	}
}

func endPasswordWork() {
	<-passwordWork
}

func serveAuthError(w http.ResponseWriter, r *http.Request, path, code, reason, username string) {
	logAuthFormRejection(r, reason)
	serveAuthPage(w, r, path, authPageValues{
		errorCode: code,
		username:  username,
	})
}

func logAuthFormRejection(r *http.Request, reason string) {
	log.Printf("auth form rejected path=%q reason=%q", r.URL.Path, reason)
}

func authUsernameForRender(value string) string {
	if len(value) > 256 {
		return ""
	}
	return value
}

func setDashboardSession(w http.ResponseWriter, r *http.Request, sessionKey string, admin *db.Admin) {
	http.SetCookie(w, &http.Cookie{
		Name:     dashboardSessionCookie,
		Value:    dashboardSessionValue(sessionKey, admin),
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func dashboardSessionValue(sessionKey string, admin *db.Admin) string {
	mac := hmac.New(sha256.New, []byte(sessionKey))
	_, _ = mac.Write([]byte("degitide-dashboard-session-v2\x00"))
	_, _ = mac.Write([]byte(admin.Username))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(admin.PasswordHash))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
