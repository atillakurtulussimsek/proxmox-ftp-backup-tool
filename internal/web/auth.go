package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
)

const AUTH_CACHE_TTL = 10 * time.Minute

// basicAuth, HTTP Basic Auth uygular. bcrypt her istekte maliyetli olduğundan başarılı
// doğrulamalar hash ile anahtarlanmış kısa süreli bir önbellekte tutulur.
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if ok && s.checkCreds(user, pass) {
			next.ServeHTTP(w, r)
			return
		}
		if ok {
			s.log.Warn("başarısız giriş denemesi", "remote", r.RemoteAddr, "user", user)
			time.Sleep(500 * time.Millisecond)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="PVE FTP Yedek", charset="UTF-8"`)
		http.Error(w, "Yetkisiz", http.StatusUnauthorized)
	})
}

func (s *Server) checkCreds(user, pass string) bool {
	cfg := s.cfg.Get()
	if cfg.Auth.PasswordHash == "" || cfg.Auth.Username == "" {
		return false
	}
	key := sha256.Sum256([]byte(cfg.Auth.PasswordHash + "\x00" + user + "\x00" + pass))
	now := time.Now()
	s.authMu.Lock()
	exp, hit := s.authCache[key]
	s.authMu.Unlock()
	if hit && now.Before(exp) {
		return true
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(cfg.Auth.Username)) == 1
	passOK := config.CheckPassword(cfg.Auth.PasswordHash, pass)
	if !userOK || !passOK {
		return false
	}
	s.authMu.Lock()
	if len(s.authCache) > 64 {
		s.authCache = map[[32]byte]time.Time{}
	}
	s.authCache[key] = now.Add(AUTH_CACHE_TTL)
	s.authMu.Unlock()
	return true
}

// csrfToken, kullanıcıya bağlı, gizli anahtarla imzalanmış token üretir.
func (s *Server) csrfToken() string {
	cfg := s.cfg.Get()
	mac := hmac.New(sha256.New, []byte(cfg.CSRFSecret))
	mac.Write([]byte("csrf:" + cfg.Auth.Username + ":" + cfg.Auth.PasswordHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// csrf, durum değiştiren isteklerde X-CSRF-Token başlığını veya csrf_token alanını doğrular.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = r.PostFormValue("csrf_token")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.csrfToken())) != 1 {
			http.Error(w, "Geçersiz CSRF token. Sayfayı yenileyin.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
