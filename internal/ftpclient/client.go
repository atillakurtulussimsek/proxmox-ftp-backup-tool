// Package ftpclient, FTP, FTPS (explicit) ve SFTP için ortak bir istemci arayüzü sağlar.
// Tüm dosya işlemleri ayarlardaki uzak kök klasöre göredir ve yalnızca bu klasördeki
// düz dosya adlarını kabul eder.
package ftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
)

type FileInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// ProgressFunc, o ana kadar aktarılan toplam bayt sayısıyla çağrılır.
type ProgressFunc func(transferred int64)

// Client, uzak sunucu işlemleri için ortak arayüzdür. Eşzamanlı kullanım için güvenli değildir.
type Client interface {
	// EnsureRoot, uzak kök klasörü yoksa oluşturur.
	EnsureRoot(ctx context.Context) error
	// List, uzak kök klasördeki dosyaları listeler (klasörler hariç).
	List(ctx context.Context) ([]FileInfo, error)
	Upload(ctx context.Context, localPath, name string, progress ProgressFunc) error
	Download(ctx context.Context, name string, w io.Writer, progress ProgressFunc) error
	Size(ctx context.Context, name string) (int64, error)
	Delete(ctx context.Context, name string) error
	// Rename, kök klasördeki bir dosyanın adını değiştirir (hedef varsa üzerine yazar).
	Rename(ctx context.Context, from, to string) error
	Close() error
}

// ErrInvalidName, güvensiz uzak dosya adı için döndürülür.
var ErrInvalidName = errors.New("geçersiz uzak dosya adı")

// ValidateName, adın kök klasör içinde düz bir dosya adı olduğunu doğrular.
func ValidateName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\\x00\r\n") || len(name) > 255 {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

// SafeJoin, adı kök klasörle birleştirir; sonuç kökün dışına çıkıyorsa hata döndürür.
func SafeJoin(root, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	cleanRoot := CleanRoot(root)
	full := path.Join(cleanRoot, name)
	if path.Dir(full) != cleanRoot {
		return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return full, nil
}

// CleanRoot, uzak kök klasörü normalize eder.
func CleanRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "/"
	}
	return path.Clean(root)
}

// New, ayarlara göre bağlanmış bir istemci döndürür. SFTP'de ilk bağlantıda
// sunucu anahtarı kaydedilmemişse kabul edilir; görülen anahtar HostKey ile alınabilir.
func New(ctx context.Context, cfg config.FTPConfig) (Client, error) {
	if cfg.Host == "" {
		return nil, errors.New("FTP sunucusu ayarlanmamış")
	}
	switch cfg.Protocol {
	case config.PROTOCOL_FTP, config.PROTOCOL_FTPS:
		return dialFTP(ctx, cfg)
	case config.PROTOCOL_SFTP:
		return dialSFTP(ctx, cfg)
	default:
		return nil, fmt.Errorf("bilinmeyen protokol: %s", cfg.Protocol)
	}
}

// HostKeyReporter, SFTP istemcisinin gördüğü sunucu anahtarını bildirir.
type HostKeyReporter interface {
	HostKey() string
}

// ctxReader, bağlam iptal edildiğinde okumayı keser ve ilerlemeyi bildirir.
type ctxReader struct {
	ctx      context.Context
	r        io.Reader
	n        int64
	progress ProgressFunc
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.progress != nil {
			c.progress(c.n)
		}
	}
	return n, err
}

// copyCtx, iptal destekli ve ilerleme bildirimli kopyalama yapar.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader, progress ProgressFunc) (int64, error) {
	buf := make([]byte, 256*1024)
	return io.CopyBuffer(dst, &ctxReader{ctx: ctx, r: src, progress: progress}, buf)
}
