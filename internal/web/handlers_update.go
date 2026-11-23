package web

import (
	"context"
	"net/http"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/updater"
)

type updateView struct {
	Current string
	Rel     updater.Release
	Newer   bool
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := updater.Latest(r.Context())
	if err != nil {
		s.flash(w, false, "Kontrol edilemedi: %v", err)
		return
	}
	s.fragment(w, "updateResult", updateView{Current: s.version, Rel: rel, Newer: updater.IsNewer(s.version, rel.Tag)})
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	tag := r.PostFormValue("tag")
	release, err := s.runner.HoldForUpdate()
	if err != nil {
		s.flash(w, false, "Güncellenemedi: %v", err)
		return
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	rel, err := updater.Latest(ctx)
	if err != nil {
		s.flash(w, false, "Güncellenemedi: %v", err)
		return
	}
	if rel.Tag != tag || !updater.IsNewer(s.version, rel.Tag) {
		s.flash(w, false, "Son sürüm değişmiş (%s); tekrar kontrol edin.", rel.Tag)
		return
	}
	exe, err := updater.Executable()
	if err != nil {
		s.flash(w, false, "Binary yolu bulunamadı: %v", err)
		return
	}
	s.log.Info("güncelleme kuruluyor", "from", s.version, "to", rel.Tag, "exe", exe)
	if err := updater.Apply(ctx, rel, exe); err != nil {
		s.log.Error("güncelleme başarısız", "err", err)
		s.flash(w, false, "Güncellenemedi: %v", err)
		return
	}
	s.log.Info("güncelleme kuruldu", "version", rel.Tag)

	// Yeni binary diskte; servis yeniden başlayana kadar yeni iş başlatılmasın.
	held = false
	if !updater.UnderSystemd() {
		s.flash(w, true, "%s kuruldu. Servis systemd altında çalışmadığı için uygulamayı elle yeniden başlatın.", rel.Tag)
		return
	}
	go func() {
		time.Sleep(time.Second)
		if err := updater.Restart(); err != nil {
			s.log.Error("servis yeniden başlatılamadı", "err", err)
			release()
		}
	}()
	s.fragment(w, "updateRestarting", rel.Tag)
}

// handleUpdateWait, yeni sürüm ayağa kalkınca sayfayı yeniler.
func (s *Server) handleUpdateWait(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("tag") == s.version {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}
