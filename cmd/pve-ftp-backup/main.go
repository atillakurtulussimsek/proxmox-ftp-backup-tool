// Komut pve-ftp-backup, Proxmox VE yedeklerini FTP/FTPS/SFTP sunucusuna aktaran
// web tabanlı yönetim aracıdır.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/scheduler"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/web"
)

var version = "dev"

const MIN_PASSWORD_LEN = 8

func usage() {
	fmt.Fprintf(os.Stderr, `Kullanım: pve-ftp-backup <komut> [seçenekler]

Komutlar:
  serve          Web arayüzünü ve zamanlayıcıyı başlatır (varsayılan)
  set-password   Arayüz kullanıcı adını ve şifresini belirler
  version        Sürümü yazdırır

Seçenekler:
  -config yol    Ayar dosyası (varsayılan %s)
  -db yol        Veritabanı dosyası, yalnızca serve (varsayılan %s)
`, config.DEFAULT_CONFIG_PATH, config.DEFAULT_DB_PATH)
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = usage
	cfgPath := fs.String("config", config.DEFAULT_CONFIG_PATH, "ayar dosyası")
	dbPath := fs.String("db", config.DEFAULT_DB_PATH, "veritabanı dosyası")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "serve":
		err = serve(*cfgPath, *dbPath)
	case "set-password":
		err = setPassword(*cfgPath)
	case "version", "-v", "--version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Hata:", err)
		os.Exit(1)
	}
}

func serve(cfgPath, dbPath string) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cfg := cfgMgr.Get()
	if cfg.Auth.PasswordHash == "" {
		return errors.New("arayüz şifresi belirlenmemiş; önce 'pve-ftp-backup set-password' çalıştırın")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.MarkInterrupted(); err != nil {
		logger.Warn("yarım kalan işler işaretlenemedi", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := backup.NewRunner(context.Background(), cfgMgr, st, logger)
	sched := scheduler.New(func() {
		c := cfgMgr.Get()
		if _, err := runner.StartBackup(store.TRIGGER_SCHEDULED, c.Backup.VMIDs); err != nil {
			logger.Warn("zamanlanmış yedekleme başlatılamadı", "err", err)
		}
	})
	if err := sched.Reload(cfg.Schedule.Enabled, cfg.Schedule.Cron); err != nil {
		logger.Warn("zamanlama yüklenemedi", "err", err)
	}

	go housekeeping(ctx, cfgMgr, st, logger)

	srv, err := web.New(cfgMgr, st, runner, sched, logger, version)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		useTLS := cfg.TLSCert != "" && cfg.TLSKey != ""
		logger.Info("web arayüzü başlatılıyor", "addr", cfg.Listen, "tls", useTLS, "version", version)
		var err error
		if useTLS {
			err = httpSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("kapatılıyor")
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("web sunucusu: %w", err)
		}
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shCtx)
	sched.Stop()
	runner.Shutdown(3 * time.Minute)
	return nil
}

// housekeeping, saklama süresi dolan geçmiş kayıtlarını periyodik olarak siler.
func housekeeping(ctx context.Context, cfg *config.Manager, st *store.Store, logger *slog.Logger) {
	run := func() {
		days := cfg.Get().LogRetentionDays
		n, err := st.Purge(time.Now().AddDate(0, 0, -days))
		if err != nil {
			logger.Warn("eski kayıtlar silinemedi", "err", err)
		} else if n > 0 {
			logger.Info("eski iş kayıtları silindi", "count", n, "days", days)
		}
	}
	run()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func setPassword(cfgPath string) error {
	cfgMgr, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	cur := cfgMgr.Get().Auth.Username
	if cur == "" {
		cur = "admin"
	}
	in := bufio.NewReader(os.Stdin)
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	if interactive {
		fmt.Printf("Kullanıcı adı [%s]: ", cur)
	}
	user, err := readLine(in)
	if err != nil {
		return err
	}
	if user == "" {
		user = cur
	}
	if strings.ContainsAny(user, ": \t") {
		return errors.New("kullanıcı adı boşluk veya ':' içeremez")
	}

	pass, err := readSecret(in, interactive, "Şifre: ")
	if err != nil {
		return err
	}
	if len(pass) < MIN_PASSWORD_LEN {
		return fmt.Errorf("şifre en az %d karakter olmalı", MIN_PASSWORD_LEN)
	}
	again, err := readSecret(in, interactive, "Şifre (tekrar): ")
	if err != nil {
		return err
	}
	if pass != again {
		return errors.New("şifreler eşleşmiyor")
	}
	hash, err := config.HashPassword(pass)
	if err != nil {
		return err
	}
	if err := cfgMgr.Update(func(c *config.Config) error {
		c.Auth.Username = user
		c.Auth.PasswordHash = hash
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("Şifre kaydedildi (kullanıcı: %s). Servis çalışıyorsa yeni şifre hemen geçerlidir.\n", user)
	return nil
}

func readLine(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("girdi okunamadı: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readSecret(in *bufio.Reader, interactive bool, prompt string) (string, error) {
	if !interactive {
		return readLine(in)
	}
	fmt.Print(prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return string(b), nil
}
