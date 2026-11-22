package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/notify"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/scheduler"
)

type settingsData struct {
	Cfg         config.Config
	HasPassword bool
	HasToken    bool
	CronDesc    string
	NextRuns    []time.Time
	CronErr     string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	d := settingsData{
		Cfg:         cfg,
		HasPassword: cfg.FTP.Password != "",
		HasToken:    cfg.Telegram.Token != "",
		CronDesc:    scheduler.Describe(cfg.Schedule.Cron),
	}
	// Gizli alanlar şablona hiçbir zaman gitmez.
	d.Cfg.FTP.Password = ""
	d.Cfg.Telegram.Token = ""
	d.Cfg.Auth.PasswordHash = ""
	d.Cfg.CSRFSecret = ""
	if next, err := scheduler.NextRuns(cfg.Schedule.Cron, 3); err != nil {
		d.CronErr = err.Error()
	} else {
		d.NextRuns = next
	}
	s.render(w, r, "settings", "Ayarlar", d)
}

func (s *Server) handleSettingsGuests(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "settingsGuests", s.guestRows(r.Context()))
}

func checkbox(r *http.Request, name string) bool {
	v := r.PostFormValue(name)
	return v == "on" || v == "1" || v == "true"
}

// ftpFromForm, formdaki FTP alanlarını mevcut ayarların üzerine uygular.
func ftpFromForm(r *http.Request, cur config.FTPConfig) (config.FTPConfig, error) {
	f := cur
	f.Protocol = r.PostFormValue("protocol")
	switch f.Protocol {
	case config.PROTOCOL_FTP, config.PROTOCOL_FTPS, config.PROTOCOL_SFTP:
	default:
		return f, errors.New("geçersiz protokol")
	}
	f.Host = strings.TrimSpace(r.PostFormValue("host"))
	if f.Host == "" || strings.ContainsAny(f.Host, " /\\@") {
		return f, errors.New("geçerli bir sunucu adresi girin")
	}
	if p := strings.TrimSpace(r.PostFormValue("port")); p == "" {
		f.Port = config.DefaultPort(f.Protocol)
	} else {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return f, errors.New("port 1-65535 arasında olmalı")
		}
		f.Port = port
	}
	f.Username = strings.TrimSpace(r.PostFormValue("username"))
	if f.Username == "" {
		return f, errors.New("kullanıcı adı gerekli")
	}
	if pw := r.PostFormValue("password"); pw != "" {
		f.Password = pw
	}
	dir := strings.TrimSpace(r.PostFormValue("remoteDir"))
	if strings.ContainsAny(dir, "\x00\r\n") {
		return f, errors.New("geçersiz uzak klasör")
	}
	f.RemoteDir = ftpclient.CleanRoot(dir)
	f.Passive = checkbox(r, "passive")
	f.DisableEPSV = checkbox(r, "disableEpsv")
	f.TLSInsecure = checkbox(r, "tlsInsecure")
	if f.Host != cur.Host || f.Port != cur.Port || f.Protocol != cur.Protocol || checkbox(r, "resetHostKey") {
		f.HostKey = ""
	}
	return f, nil
}

func (s *Server) handleSaveFTP(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.Update(func(c *config.Config) error {
		f, err := ftpFromForm(r, c.FTP)
		if err != nil {
			return err
		}
		c.FTP = f
		return nil
	})
	if err != nil {
		s.flash(w, false, "Kaydedilemedi: %v", err)
		return
	}
	s.flash(w, true, "FTP ayarları kaydedildi.")
}

func (s *Server) handleTestFTP(w http.ResponseWriter, r *http.Request) {
	cur := s.cfg.Get().FTP
	f, err := ftpFromForm(r, cur)
	if err != nil {
		s.flash(w, false, "%v", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	msg, err := testFTP(ctx, f)
	if err != nil {
		s.flash(w, false, "Bağlantı testi başarısız: %v", err)
		return
	}
	s.flash(w, true, "%s", msg)
}

func testFTP(ctx context.Context, f config.FTPConfig) (string, error) {
	c, err := ftpclient.New(ctx, f)
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.EnsureRoot(ctx); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "pve-ftp-test-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	content := []byte("pve-ftp-backup bağlantı testi " + time.Now().Format(time.RFC3339) + "\n")
	_, werr := tmp.Write(content)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	name := ".pve-ftp-backup-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".txt"
	if err := c.Upload(ctx, tmp.Name(), name, nil); err != nil {
		return "", fmt.Errorf("test dosyası yazılamadı: %w", err)
	}
	size, err := c.Size(ctx, name)
	if err != nil {
		_ = c.Delete(ctx, name)
		return "", fmt.Errorf("test dosyası doğrulanamadı: %w", err)
	}
	if size != int64(len(content)) {
		_ = c.Delete(ctx, name)
		return "", fmt.Errorf("test dosyası boyutu uyuşmuyor (%d / %d)", size, len(content))
	}
	if err := c.Delete(ctx, name); err != nil {
		return "", fmt.Errorf("test dosyası silinemedi: %w", err)
	}
	msg := "Bağlantı başarılı. Uzak klasör erişilebilir; test dosyası yazılıp silindi."
	if hk, ok := c.(ftpclient.HostKeyReporter); ok && hk.HostKey() != "" {
		if f.HostKey == "" {
			msg += " Sunucu anahtarı: " + hk.HostKey() + " (ilk kayıtlı bağlantıda saklanacak)"
		} else {
			msg += " Sunucu anahtarı doğrulandı."
		}
	}
	return msg, nil
}

var validModes = map[string]bool{"snapshot": true, "suspend": true, "stop": true}
var validCompress = map[string]bool{"zstd": true, "gzip": true, "lzo": true}

func (s *Server) handleSaveBackup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.flash(w, false, "Form okunamadı")
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		mode := r.PostFormValue("mode")
		if !validModes[mode] {
			return errors.New("geçersiz mod")
		}
		comp := r.PostFormValue("compress")
		if !validCompress[comp] {
			return errors.New("geçersiz sıkıştırma")
		}
		dir := strings.TrimSpace(r.PostFormValue("dumpDir"))
		if !filepath.IsAbs(dir) {
			return errors.New("geçici klasör mutlak yol olmalı")
		}
		keep, err := strconv.Atoi(r.PostFormValue("defaultKeep"))
		if err != nil || keep < 1 || keep > 1000 {
			return errors.New("varsayılan KEEP 1-1000 arasında olmalı")
		}
		c.Backup.Mode, c.Backup.Compress, c.Backup.DumpDir, c.Backup.DefaultKeep = mode, comp, filepath.Clean(dir), keep

		// VM listesi yüklenemediyse seçimlere ve VM bazlı KEEP değerlerine dokunma.
		if r.PostFormValue("guestsLoaded") != "1" {
			return nil
		}
		var ids []int
		for _, v := range r.PostForm["vmid"] {
			id, err := strconv.Atoi(v)
			if err != nil || id <= 0 {
				return fmt.Errorf("geçersiz VM ID: %q", v)
			}
			ids = append(ids, id)
		}
		c.Backup.VMIDs = ids
		perVM := map[int]int{}
		for k, vals := range r.PostForm {
			if !strings.HasPrefix(k, "keep_") || len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
				continue
			}
			id, err := strconv.Atoi(strings.TrimPrefix(k, "keep_"))
			if err != nil || id <= 0 {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(vals[0]))
			if err != nil || n < 1 || n > 1000 {
				return fmt.Errorf("VM %d için KEEP 1-1000 arasında olmalı", id)
			}
			perVM[id] = n
		}
		// Listede olmayan (başka node'a taşınmış/silinmiş) VM'lerin değerlerini koru.
		listed := map[int]bool{}
		for _, v := range r.PostForm["listed"] {
			if id, err := strconv.Atoi(v); err == nil {
				listed[id] = true
			}
		}
		for id, n := range c.Backup.KeepPerVM {
			if !listed[id] {
				perVM[id] = n
			}
		}
		c.Backup.KeepPerVM = perVM
		return nil
	})
	if err != nil {
		s.flash(w, false, "Kaydedilemedi: %v", err)
		return
	}
	s.flash(w, true, "Yedekleme ayarları kaydedildi.")
}

func (s *Server) handleSaveSchedule(w http.ResponseWriter, r *http.Request) {
	enabled := checkbox(r, "enabled")
	spec := strings.TrimSpace(r.PostFormValue("cron"))
	if _, err := scheduler.Parse(spec); err != nil {
		s.flash(w, false, "%v", err)
		return
	}
	if err := s.cfg.Update(func(c *config.Config) error {
		c.Schedule.Enabled = enabled
		c.Schedule.Cron = spec
		return nil
	}); err != nil {
		s.flash(w, false, "Kaydedilemedi: %v", err)
		return
	}
	if err := s.sched.Reload(enabled, spec); err != nil {
		s.flash(w, false, "Zamanlama uygulanamadı: %v", err)
		return
	}
	if !enabled {
		s.flash(w, true, "Zamanlama kaydedildi (kapalı).")
		return
	}
	s.flash(w, true, "Zamanlama kaydedildi: %s. Sonraki çalışma: %s", scheduler.Describe(spec), s.sched.Next().Format("02.01.2006 15:04"))
}

func telegramFromForm(r *http.Request, cur config.TelegramConfig) config.TelegramConfig {
	t := cur
	if tok := strings.TrimSpace(r.PostFormValue("token")); tok != "" {
		t.Token = tok
	}
	t.ChatID = strings.TrimSpace(r.PostFormValue("chatId"))
	t.OnSuccess = checkbox(r, "onSuccess")
	t.OnFailure = checkbox(r, "onFailure")
	t.OnCleanup = checkbox(r, "onCleanup")
	return t
}

func (s *Server) handleSaveTelegram(w http.ResponseWriter, r *http.Request) {
	clear := checkbox(r, "clearToken")
	err := s.cfg.Update(func(c *config.Config) error {
		c.Telegram = telegramFromForm(r, c.Telegram)
		if clear {
			c.Telegram.Token = ""
		}
		return nil
	})
	if err != nil {
		s.flash(w, false, "Kaydedilemedi: %v", err)
		return
	}
	s.flash(w, true, "Telegram ayarları kaydedildi.")
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	t := telegramFromForm(r, s.cfg.Get().Telegram)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg := "🔔 <b>Test mesajı</b>\nNode: " + notify.Escape(backup.LocalNode()) + "\nPVE FTP Yedek bildirimleri çalışıyor."
	if err := notify.SendTelegram(ctx, t, msg); err != nil {
		s.flash(w, false, "%v", err)
		return
	}
	s.flash(w, true, "Test mesajı gönderildi.")
}

func (s *Server) handleSaveGeneral(w http.ResponseWriter, r *http.Request) {
	days, err := strconv.Atoi(r.PostFormValue("logRetentionDays"))
	if err != nil || days < 1 || days > 3650 {
		s.flash(w, false, "Saklama süresi 1-3650 gün arasında olmalı")
		return
	}
	if err := s.cfg.Update(func(c *config.Config) error {
		c.LogRetentionDays = days
		return nil
	}); err != nil {
		s.flash(w, false, "Kaydedilemedi: %v", err)
		return
	}
	s.flash(w, true, "Genel ayarlar kaydedildi.")
}
