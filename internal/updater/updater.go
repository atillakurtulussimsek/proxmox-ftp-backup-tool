// Package updater, GitHub Releases üzerinden yeni sürümü kontrol eder ve çalışan
// binary'yi SHA256 doğrulamasıyla değiştirir.
package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	REPO          = "atillakurtulussimsek/proxmox-ftp-backup-tool"
	ASSET_BINARY  = "pve-ftp-backup-linux-amd64"
	ASSET_SUMS    = "SHA256SUMS"
	SERVICE_NAME  = "pve-ftp-backup.service"
	MAX_BIN_SIZE  = 200 << 20
	MAX_SUMS_SIZE = 64 << 10
)

// API_BASE testlerde değiştirilebilir.
var API_BASE = "https://api.github.com"

// Release, GitHub'daki son kararlı sürümdür.
type Release struct {
	Tag       string
	Name      string
	Notes     string
	URL       string
	Published time.Time
	binURL    string
	sumsURL   string
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

var errNotFound = errors.New("HTTP 404")

var client = &http.Client{Timeout: 10 * time.Minute}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pve-ftp-backup-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// Latest, son kararlı (ön sürüm olmayan) release'i getirir.
func Latest(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := get(ctx, API_BASE+"/repos/"+REPO+"/releases/latest")
	if errors.Is(err, errNotFound) {
		return Release{}, errors.New("henüz yayımlanmış sürüm yok")
	}
	if err != nil {
		return Release{}, fmt.Errorf("GitHub'a ulaşılamadı: %w", err)
	}
	defer resp.Body.Close()
	var gr ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&gr); err != nil {
		return Release{}, fmt.Errorf("GitHub yanıtı okunamadı: %w", err)
	}
	rel := Release{Tag: gr.TagName, Name: gr.Name, Notes: gr.Body, URL: gr.HTMLURL, Published: gr.PublishedAt}
	for _, a := range gr.Assets {
		switch a.Name {
		case ASSET_BINARY:
			rel.binURL = a.URL
		case ASSET_SUMS:
			rel.sumsURL = a.URL
		}
	}
	if _, ok := ParseVersion(rel.Tag); !ok {
		return Release{}, fmt.Errorf("geçersiz sürüm etiketi: %q", rel.Tag)
	}
	return rel, nil
}

// Installable, release'te kurulum için gereken dosyalar var mı.
func (r Release) Installable() bool { return r.binURL != "" && r.sumsURL != "" }

// Version, vMAJOR.MINOR.PATCH biçimindeki sürümdür.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseVersion, "v1.2.3" veya "v1.2.3-rc1" biçimini çözer.
func ParseVersion(s string) (Version, bool) {
	s, ok := strings.CutPrefix(strings.TrimSpace(s), "v")
	if !ok {
		return Version{}, false
	}
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" {
			return Version{}, false
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2], pre}, true
}

// Less, a sürümü b'den eski mi.
func (a Version) Less(b Version) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	if a.Patch != b.Patch {
		return a.Patch < b.Patch
	}
	// Ön sürüm, aynı numaralı kararlı sürümden eskidir.
	if a.Pre == "" || b.Pre == "" {
		return a.Pre != "" && b.Pre == ""
	}
	return a.Pre < b.Pre
}

// IsNewer, latest etiketi current'tan yeni mi. current bir sürüm etiketi değilse
// (geliştirme derlemesi) her geçerli release yeni sayılır.
func IsNewer(current, latest string) bool {
	l, ok := ParseVersion(latest)
	if !ok {
		return false
	}
	c, ok := ParseVersion(current)
	if !ok {
		return true
	}
	return c.Less(l)
}

// Apply, release'teki binary'yi indirir, SHA256SUMS ile doğrular, çalıştırıp sürümünü
// kontrol eder ve exe dosyasının yerine atomik olarak koyar.
func Apply(ctx context.Context, rel Release, exe string) error {
	if !rel.Installable() {
		return errors.New("release'te binary veya SHA256SUMS dosyası yok")
	}
	want, err := expectedSum(ctx, rel.sumsURL)
	if err != nil {
		return err
	}

	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".pve-ftp-backup-update-*")
	if err != nil {
		return fmt.Errorf("geçici dosya oluşturulamadı: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tmpName)
		}
	}()

	resp, err := get(ctx, rel.binURL)
	if err != nil {
		return fmt.Errorf("binary indirilemedi: %w", err)
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, MAX_BIN_SIZE+1))
	if err != nil {
		return fmt.Errorf("binary indirilemedi: %w", err)
	}
	if n > MAX_BIN_SIZE {
		return errors.New("binary beklenenden büyük")
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA256 uyuşmuyor (beklenen %s, inen %s)", want, got)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}

	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, tmpName, "version").Output()
	if err != nil {
		return fmt.Errorf("yeni binary çalıştırılamadı: %w", err)
	}
	if v := strings.TrimSpace(string(out)); v != rel.Tag {
		return fmt.Errorf("yeni binary sürümü %q, beklenen %q", v, rel.Tag)
	}

	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("binary değiştirilemedi: %w", err)
	}
	ok = true
	return nil
}

func expectedSum(ctx context.Context, url string) (string, error) {
	resp, err := get(ctx, url)
	if err != nil {
		return "", fmt.Errorf("SHA256SUMS indirilemedi: %w", err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, MAX_SUMS_SIZE))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == ASSET_BINARY {
			sum := strings.ToLower(f[0])
			if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
				return "", errors.New("SHA256SUMS içindeki özet geçersiz")
			}
			return sum, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("SHA256SUMS içinde %s yok", ASSET_BINARY)
}

// Executable, çalışan binary'nin gerçek yolunu döndürür.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// UnderSystemd, süreç systemd servisi olarak mı çalışıyor.
func UnderSystemd() bool { return os.Getenv("INVOCATION_ID") != "" }

// Restart, servisi systemd üzerinden yeniden başlatır. --no-block sayesinde komut hemen
// döner; systemd mevcut süreci SIGTERM ile düzgünce kapatıp yeni binary'yi başlatır.
func Restart() error {
	out, err := exec.Command("systemctl", "restart", "--no-block", SERVICE_NAME).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
