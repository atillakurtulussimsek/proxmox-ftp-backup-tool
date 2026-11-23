// Package web, HTMX tabanlı yönetim arayüzünü sunar.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/scheduler"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

//go:embed assets
var assets embed.FS

type Server struct {
	cfg     *config.Manager
	st      *store.Store
	runner  *backup.Runner
	sched   *scheduler.Scheduler
	log     *slog.Logger
	version string

	pages map[string]*template.Template
	base  *template.Template

	authMu    sync.Mutex
	authCache map[[32]byte]time.Time
}

func New(cfg *config.Manager, st *store.Store, runner *backup.Runner, sched *scheduler.Scheduler, log *slog.Logger, version string) (*Server, error) {
	s := &Server{
		cfg: cfg, st: st, runner: runner, sched: sched, log: log, version: version,
		pages:     map[string]*template.Template{},
		authCache: map[[32]byte]time.Time{},
	}
	base, err := template.New("").Funcs(funcMap()).ParseFS(assets, "assets/templates/layout.html", "assets/templates/partials.html")
	if err != nil {
		return nil, fmt.Errorf("şablonlar yüklenemedi: %w", err)
	}
	s.base = base
	for _, p := range []string{"dashboard", "backups", "settings", "history", "job"} {
		t, err := template.Must(base.Clone()).ParseFS(assets, "assets/templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("%s şablonu yüklenemedi: %w", p, err)
		}
		s.pages[p] = t
	}
	return s, nil
}

// Handler, tüm rotaları güvenlik katmanlarıyla birlikte döndürür.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /dashboard/guests", s.handleDashboardGuests)

	mux.HandleFunc("GET /backups", s.handleBackups)
	mux.HandleFunc("GET /backups/list", s.handleBackupList)
	mux.HandleFunc("GET /backups/download", s.handleDownload)
	mux.HandleFunc("POST /backups/delete", s.handleDelete)
	mux.HandleFunc("POST /backups/restore", s.handleRestore)

	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /settings/guests", s.handleSettingsGuests)
	mux.HandleFunc("GET /settings/cron-preview", s.handleCronPreview)
	mux.HandleFunc("POST /settings/ftp", s.handleSaveFTP)
	mux.HandleFunc("POST /settings/ftp/test", s.handleTestFTP)
	mux.HandleFunc("POST /settings/backup", s.handleSaveBackup)
	mux.HandleFunc("POST /settings/schedule", s.handleSaveSchedule)
	mux.HandleFunc("POST /settings/telegram", s.handleSaveTelegram)
	mux.HandleFunc("POST /settings/telegram/test", s.handleTestTelegram)
	mux.HandleFunc("POST /settings/general", s.handleSaveGeneral)
	mux.HandleFunc("POST /settings/bandwidth", s.handleSaveBandwidth)

	mux.HandleFunc("POST /update/check", s.handleUpdateCheck)
	mux.HandleFunc("POST /update/apply", s.handleUpdateApply)
	mux.HandleFunc("GET /update/wait", s.handleUpdateWait)

	mux.HandleFunc("POST /jobs/backup", s.handleRunBackup)
	mux.HandleFunc("POST /jobs/cleanup", s.handleRunCleanup)
	mux.HandleFunc("POST /jobs/cancel", s.handleCancel)
	mux.HandleFunc("GET /jobs/status", s.handleJobStatus)

	mux.HandleFunc("GET /history", s.handleHistory)
	mux.HandleFunc("GET /history/{id}", s.handleJob)
	mux.HandleFunc("GET /history/{id}/log", s.handleJobLog)

	cop := http.NewCrossOriginProtection()
	return s.securityHeaders(s.basicAuth(cop.Handler(s.csrf(mux))))
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// pageData, tüm sayfa şablonlarına geçirilen ortak veridir.
type pageData struct {
	Title   string
	Active  string
	CSRF    string
	Version string
	Node    string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "şablon bulunamadı", http.StatusInternalServerError)
		return
	}
	pd := pageData{
		Title: title, Active: page, CSRF: s.csrfToken(), Version: s.version,
		Node: backup.LocalNode(), Data: data,
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", pd); err != nil {
		s.log.Error("şablon hatası", "page", page, "err", err)
		http.Error(w, "şablon hatası", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

func (s *Server) fragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.base.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("şablon hatası", "fragment", name, "err", err)
		http.Error(w, "şablon hatası", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

type flash struct {
	OK  bool
	Msg string
}

func (s *Server) flash(w http.ResponseWriter, ok bool, format string, args ...any) {
	// Tüm mesajlar sayfanın sağ altındaki ortak bildirim alanında gösterilir.
	w.Header().Set("HX-Retarget", "#flash")
	w.Header().Set("HX-Reswap", "innerHTML")
	s.fragment(w, "flash", flash{OK: ok, Msg: fmt.Sprintf(format, args...)})
}

func funcMap() template.FuncMap {
	return template.FuncMap{
		"bytes": backup.HumanBytes,
		"dur":   backup.HumanDuration,
		"ts": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("02.01.2006 15:04:05")
		},
		"statusText":  backup.StatusText,
		"statusClass": statusClass,
		"kindText":    kindText,
		"triggerText": func(t string) string {
			if t == store.TRIGGER_SCHEDULED {
				return "Zamanlanmış"
			}
			return "Manuel"
		},
		"pct":      func(f float64) string { return fmt.Sprintf("%.1f", f) },
		"speed":    func(f float64) string { return backup.HumanBytes(int64(f)) + "/sn" },
		"mbit":     mbitPerSec,
		"add":      func(a, b int) int { return a + b },
		"sub":      func(a, b int) int { return a - b },
		"cronText": scheduler.Describe,
		"typeText": func(t string) string {
			if t == "lxc" {
				return "LXC"
			}
			return "VM"
		},
		"contains": func(list []int, v int) bool {
			for _, x := range list {
				if x == v {
					return true
				}
			}
			return false
		},
		"ago":   ago,
		"upper": strings.ToUpper,
		"list":  func(v ...string) []string { return v },
	}
}

// mbitPerSec, bayt/sn hızını Mbit/sn olarak yazar (float64 veya int64 kabul eder).
func mbitPerSec(v any) string {
	var bps float64
	switch n := v.(type) {
	case float64:
		bps = n
	case int64:
		bps = float64(n)
	case int:
		bps = float64(n)
	}
	return fmt.Sprintf("%.1f Mbit/sn", bps*8/1_000_000)
}

// ago, zamanı "3 sa önce" biçiminde yazar.
func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return t.Local().Format("02.01 15:04")
	case d < time.Minute:
		return "az önce"
	case d < time.Hour:
		return fmt.Sprintf("%d dk önce", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d sa önce", int(d.Hours()))
	}
	return fmt.Sprintf("%d gün önce", int(d.Hours()/24))
}

func statusClass(s string) string {
	switch s {
	case store.STATUS_SUCCESS:
		return "ok"
	case store.STATUS_FAILED:
		return "err"
	case store.STATUS_PARTIAL, store.STATUS_SKIPPED, store.STATUS_REJECTED:
		return "warn"
	case store.STATUS_RUNNING:
		return "run"
	}
	return "muted"
}

func kindText(k string) string {
	switch k {
	case store.KIND_BACKUP:
		return "Yedekleme"
	case store.KIND_CLEANUP:
		return "Temizlik"
	case store.KIND_RESTORE:
		return "Geri alma"
	}
	return k
}
