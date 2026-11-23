// Package config, uygulama ayarlarının okunması, doğrulanması ve kalıcı olarak
// saklanmasından sorumludur.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	DEFAULT_CONFIG_PATH = "/etc/pve-ftp-backup/config.json"
	DEFAULT_DB_PATH     = "/var/lib/pve-ftp-backup/data.db"
	DEFAULT_LISTEN      = "0.0.0.0:8090"
	DEFAULT_DUMP_DIR    = "/var/lib/vz/dump"
	DEFAULT_KEEP        = 7
	DEFAULT_LOG_DAYS    = 90
)

// Protokol değerleri.
const (
	PROTOCOL_FTP  = "ftp"
	PROTOCOL_FTPS = "ftps"
	PROTOCOL_SFTP = "sftp"
)

type AuthConfig struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

type FTPConfig struct {
	Protocol    string `json:"protocol"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	RemoteDir   string `json:"remoteDir"`
	Passive     bool   `json:"passive"`
	DisableEPSV bool   `json:"disableEpsv"`
	// TLSInsecure, FTPS sunucusunun sertifika doğrulamasını atlar (kendinden imzalı sertifikalar için).
	TLSInsecure bool `json:"tlsInsecure"`
	// HostKey, SFTP sunucusunun ilk bağlantıda kaydedilen SSH anahtar parmak izidir (SHA256).
	HostKey string `json:"hostKey"`
}

type BackupConfig struct {
	VMIDs       []int       `json:"vmids"`
	Mode        string      `json:"mode"`
	Compress    string      `json:"compress"`
	DumpDir     string      `json:"dumpDir"`
	DefaultKeep int         `json:"defaultKeep"`
	KeepPerVM   map[int]int `json:"keepPerVm"`
}

type ScheduleConfig struct {
	Enabled bool   `json:"enabled"`
	Cron    string `json:"cron"`
}

type TelegramConfig struct {
	Token     string `json:"token"`
	ChatID    string `json:"chatId"`
	OnSuccess bool   `json:"onSuccess"`
	OnFailure bool   `json:"onFailure"`
	OnCleanup bool   `json:"onCleanup"`
}

type Config struct {
	Listen           string         `json:"listen"`
	TLSCert          string         `json:"tlsCert"`
	TLSKey           string         `json:"tlsKey"`
	CSRFSecret       string         `json:"csrfSecret"`
	Auth             AuthConfig     `json:"auth"`
	FTP              FTPConfig      `json:"ftp"`
	Backup           BackupConfig   `json:"backup"`
	Schedule         ScheduleConfig `json:"schedule"`
	Telegram         TelegramConfig `json:"telegram"`
	Bandwidth        BandwidthLimit `json:"bandwidth"`
	LogRetentionDays int            `json:"logRetentionDays"`
}

// BandwidthLimit, aktarım hız sınırlarıdır (Mbit/sn); 0 sınırsız demektir.
type BandwidthLimit struct {
	UploadMbit   int `json:"uploadMbit"`
	DownloadMbit int `json:"downloadMbit"`
}

// BytesPerSec, Mbit/sn değerini bayt/sn'ye çevirir; 0 veya negatif sınırsızdır.
func BytesPerSec(mbit int) int64 {
	if mbit <= 0 {
		return 0
	}
	return int64(mbit) * 1_000_000 / 8
}

// Default, varsayılan ayarlarla yeni bir Config döndürür.
func Default() Config {
	return Config{
		Listen:     DEFAULT_LISTEN,
		CSRFSecret: randomHex(32),
		FTP: FTPConfig{
			Protocol:  PROTOCOL_FTP,
			Port:      21,
			RemoteDir: "/",
			Passive:   true,
		},
		Backup: BackupConfig{
			Mode:        "snapshot",
			Compress:    "zstd",
			DumpDir:     DEFAULT_DUMP_DIR,
			DefaultKeep: DEFAULT_KEEP,
			KeepPerVM:   map[int]int{},
		},
		Schedule: ScheduleConfig{Cron: "0 2 * * *"},
		Telegram: TelegramConfig{
			OnSuccess: true,
			OnFailure: true,
			OnCleanup: false,
		},
		LogRetentionDays: DEFAULT_LOG_DAYS,
	}
}

// KeepFor, verilen VM için geçerli KEEP değerini döndürür.
func (c Config) KeepFor(vmid int) int {
	if v, ok := c.Backup.KeepPerVM[vmid]; ok && v > 0 {
		return v
	}
	if c.Backup.DefaultKeep > 0 {
		return c.Backup.DefaultKeep
	}
	return DEFAULT_KEEP
}

// FTPConfigured, FTP bağlantısı için asgari alanların dolu olup olmadığını söyler.
func (c Config) FTPConfigured() bool {
	return c.FTP.Host != "" && c.FTP.Username != ""
}

// Clone, map ve slice alanları dahil derin kopya üretir.
func (c Config) Clone() Config {
	out := c
	out.Backup.VMIDs = append([]int(nil), c.Backup.VMIDs...)
	out.Backup.KeepPerVM = make(map[int]int, len(c.Backup.KeepPerVM))
	for k, v := range c.Backup.KeepPerVM {
		out.Backup.KeepPerVM[k] = v
	}
	return out
}

func (c *Config) applyDefaults() {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.CSRFSecret == "" {
		c.CSRFSecret = d.CSRFSecret
	}
	if c.FTP.Protocol == "" {
		c.FTP.Protocol = d.FTP.Protocol
	}
	if c.FTP.Port == 0 {
		c.FTP.Port = DefaultPort(c.FTP.Protocol)
	}
	if c.FTP.RemoteDir == "" {
		c.FTP.RemoteDir = "/"
	}
	if c.Backup.Mode == "" {
		c.Backup.Mode = d.Backup.Mode
	}
	if c.Backup.Compress == "" {
		c.Backup.Compress = d.Backup.Compress
	}
	if c.Backup.DumpDir == "" {
		c.Backup.DumpDir = d.Backup.DumpDir
	}
	if c.Backup.DefaultKeep <= 0 {
		c.Backup.DefaultKeep = DEFAULT_KEEP
	}
	if c.Backup.KeepPerVM == nil {
		c.Backup.KeepPerVM = map[int]int{}
	}
	if c.Schedule.Cron == "" {
		c.Schedule.Cron = d.Schedule.Cron
	}
	if c.LogRetentionDays <= 0 {
		c.LogRetentionDays = DEFAULT_LOG_DAYS
	}
}

// DefaultPort, protokole göre varsayılan portu döndürür.
func DefaultPort(protocol string) int {
	if protocol == PROTOCOL_SFTP {
		return 22
	}
	return 21
}

// Manager, eşzamanlı erişime karşı korumalı ayar deposudur.
type Manager struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Load, ayar dosyasını okur. Dosya yoksa varsayılanlarla oluşturur.
func Load(path string) (*Manager, error) {
	m := &Manager{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		m.cfg = Default()
		if err := m.save(); err != nil {
			return nil, err
		}
		return m, nil
	case err != nil:
		return nil, fmt.Errorf("ayar dosyası okunamadı: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("ayar dosyası çözümlenemedi: %w", err)
	}
	cfg.applyDefaults()
	m.cfg = cfg
	return m, nil
}

// Get, ayarların bir kopyasını döndürür.
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Clone()
}

// Update, ayarları değiştirir ve diske yazar. fn hata döndürürse değişiklik uygulanmaz.
func (m *Manager) Update(fn func(c *Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.cfg.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	prev := m.cfg
	m.cfg = next
	if err := m.save(); err != nil {
		m.cfg = prev
		return err
	}
	return nil
}

func (m *Manager) save() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ayar klasörü oluşturulamadı: %w", err)
	}
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("geçici ayar dosyası oluşturulamadı: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, m.path); err != nil {
		return fmt.Errorf("ayar dosyası yazılamadı: %w", err)
	}
	return nil
}

// HashPassword, şifreyi bcrypt ile özetler.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword, şifrenin hash ile eşleşip eşleşmediğini kontrol eder.
func CheckPassword(hash, password string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
