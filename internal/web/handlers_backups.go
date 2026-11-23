package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
)

type fileView struct {
	Name      string
	Size      int64
	Time      time.Time
	IsArchive bool
}

type setView struct {
	Stamp string
	Time  time.Time
	Size  int64
	Files []fileView
}

type groupView struct {
	VMID int
	Type string
	Name string
	Size int64
	Keep int
	Sets []setView
}

type backupListData struct {
	Err    string
	Total  int64
	Groups []groupView
	Others []ftpclient.FileInfo
}

// loadRemote, uzak klasörü listeler ve VM bazında gruplar.
func (s *Server) loadRemote(ctx context.Context) ([]groupView, []ftpclient.FileInfo, int64, error) {
	cfg := s.cfg.Get()
	if !cfg.FTPConfigured() {
		return nil, nil, 0, errNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	client, err := backup.Connect(ctx, s.cfg)
	if err != nil {
		return nil, nil, 0, err
	}
	defer client.Close()
	list, others, err := backup.ListRemote(ctx, client)
	if err != nil {
		return nil, nil, 0, err
	}
	var total int64
	for _, f := range list {
		total += f.Size
	}
	for _, f := range others {
		total += f.Size
	}
	names := map[int]string{}
	if guests, err := backup.ListGuests(ctx); err == nil {
		for _, g := range guests {
			names[g.VMID] = g.Name
		}
	}
	groups, _ := backup.Group(list)
	out := make([]groupView, 0, len(groups))
	for _, g := range groups {
		gv := groupView{VMID: g.VMID, Type: g.Type, Name: names[g.VMID], Size: g.Size, Keep: cfg.KeepFor(g.VMID)}
		for _, st := range g.Sets {
			sv := setView{Stamp: st.Stamp, Time: st.Time, Size: st.Size}
			for _, f := range st.Files {
				t := f.ModTime
				if t.IsZero() {
					t = f.Time
				}
				sv.Files = append(sv.Files, fileView{Name: f.Name, Size: f.Size, Time: t, IsArchive: f.IsArchive()})
			}
			gv.Sets = append(gv.Sets, sv)
		}
		out = append(out, gv)
	}
	return out, others, total, nil
}

type notConfiguredError struct{}

func (notConfiguredError) Error() string {
	return "FTP ayarları eksik. Önce Ayarlar sayfasından FTP bilgilerini girin."
}

var errNotConfigured error = notConfiguredError{}

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "backups", "Yedekler", nil)
}

func (s *Server) handleBackupList(w http.ResponseWriter, r *http.Request) {
	groups, others, total, err := s.loadRemote(r.Context())
	d := backupListData{Groups: groups, Others: others, Total: total}
	if err != nil {
		d.Err = err.Error()
	}
	s.fragment(w, "backupList", d)
}

// remotePathFor, yalnızca vzdump biçimindeki güvenli dosya adlarını kabul eder ve
// dosyanın uzak sunucudaki köke göre göreli yolunu döndürür.
func remotePathFor(name string) (string, bool) {
	rp, err := backup.RemotePath(name)
	return rp, err == nil
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	rp, ok := remotePathFor(name)
	if !ok {
		http.Error(w, "Geçersiz dosya adı", http.StatusBadRequest)
		return
	}
	client, err := backup.Connect(r.Context(), s.cfg)
	if err != nil {
		http.Error(w, "FTP bağlantısı kurulamadı: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer client.Close()
	size, err := client.Size(r.Context(), rp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Cache-Control", "no-store")
	rate := func() int64 { return config.BytesPerSec(s.cfg.Get().Bandwidth.DownloadMbit) }
	if err := client.Download(ftpclient.WithRateLimit(r.Context(), rate), rp, w, nil); err != nil {
		s.log.Warn("indirme yarıda kaldı", "name", name, "err", err)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	rp, ok := remotePathFor(name)
	if !ok {
		s.flash(w, false, "Geçersiz dosya adı")
		return
	}
	client, err := backup.Connect(r.Context(), s.cfg)
	if err != nil {
		s.flash(w, false, "FTP bağlantısı kurulamadı: %v", err)
		return
	}
	err = client.Delete(r.Context(), rp)
	client.Close()
	if err != nil {
		s.flash(w, false, "%s silinemedi: %v", name, err)
		return
	}
	s.log.Info("uzak yedek silindi", "path", rp, "remote", r.RemoteAddr)
	w.Header().Set("HX-Trigger", "backupsChanged")
	s.flash(w, true, "%s silindi.", name)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if _, ok := remotePathFor(name); !ok {
		s.flash(w, false, "Geçersiz dosya adı")
		return
	}
	id, err := s.runner.StartRestore(name)
	if err != nil {
		s.flash(w, false, "Geri alma başlatılamadı: %v", err)
		return
	}
	w.Header().Set("HX-Trigger", "jobChanged")
	s.flash(w, true, "İndirme başladı (iş #%d). Bittiğinde dosya %s klasöründe olacak; Proxmox arayüzünden Restore ile geri yükleyebilirsiniz.", id, s.cfg.Get().Backup.DumpDir)
}
