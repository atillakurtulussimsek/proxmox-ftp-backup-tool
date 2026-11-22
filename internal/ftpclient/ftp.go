package ftpclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
)

type ftpClient struct {
	conn *ftp.ServerConn
	root string
}

func dialFTP(ctx context.Context, cfg config.FTPConfig) (*ftpClient, error) {
	if !cfg.Passive {
		return nil, fmt.Errorf("aktif FTP modu desteklenmiyor; pasif modu açın")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	opts := []ftp.DialOption{
		ftp.DialWithContext(ctx),
		ftp.DialWithTimeout(30 * time.Second),
		ftp.DialWithDisabledEPSV(cfg.DisableEPSV),
	}
	if cfg.Protocol == config.PROTOCOL_FTPS {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{
			ServerName:         cfg.Host,
			InsecureSkipVerify: cfg.TLSInsecure, //nolint:gosec // kullanıcı tercihi
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(8),
		}))
	}
	conn, err := ftp.Dial(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("sunucuya bağlanılamadı: %w", err)
	}
	if err := conn.Login(cfg.Username, cfg.Password); err != nil {
		conn.Quit()
		return nil, fmt.Errorf("giriş başarısız: %w", err)
	}
	return &ftpClient{conn: conn, root: CleanRoot(cfg.RemoteDir)}, nil
}

func (c *ftpClient) EnsureRoot(ctx context.Context) error {
	if c.root == "/" || c.root == "." {
		return nil
	}
	if err := c.conn.ChangeDir(c.root); err == nil {
		return nil
	}
	cur := ""
	if strings.HasPrefix(c.root, "/") {
		cur = "/"
	}
	for _, part := range strings.Split(strings.Trim(c.root, "/"), "/") {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur = path.Join(cur, part)
		if err := c.conn.ChangeDir(cur); err == nil {
			continue
		}
		if err := c.conn.MakeDir(cur); err != nil {
			return fmt.Errorf("uzak klasör oluşturulamadı (%s): %w", cur, err)
		}
	}
	if err := c.conn.ChangeDir(c.root); err != nil {
		return fmt.Errorf("uzak klasöre geçilemedi: %w", err)
	}
	return nil
}

func (c *ftpClient) List(ctx context.Context) ([]FileInfo, error) {
	entries, err := c.conn.List(c.root)
	if err != nil {
		return nil, fmt.Errorf("uzak klasör listelenemedi: %w", err)
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if e.Type != ftp.EntryTypeFile {
			continue
		}
		name := path.Base(e.Name)
		if ValidateName(name) != nil {
			continue
		}
		out = append(out, FileInfo{Name: name, Size: int64(e.Size), ModTime: e.Time})
	}
	return out, nil
}

func (c *ftpClient) Upload(ctx context.Context, localPath, name string, progress ProgressFunc) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := c.conn.Stor(full, &ctxReader{ctx: ctx, r: f, progress: progress}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("yükleme başarısız: %w", err)
	}
	return nil
}

func (c *ftpClient) Download(ctx context.Context, name string, w io.Writer, progress ProgressFunc) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	resp, err := c.conn.Retr(full)
	if err != nil {
		return fmt.Errorf("indirme başlatılamadı: %w", err)
	}
	_, cerr := copyCtx(ctx, w, resp, progress)
	closeErr := resp.Close()
	if cerr != nil {
		return cerr
	}
	return closeErr
}

func (c *ftpClient) Size(ctx context.Context, name string) (int64, error) {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return 0, err
	}
	n, err := c.conn.FileSize(full)
	if err == nil {
		return n, nil
	}
	// SIZE desteklemeyen sunucular için listeden bak.
	files, lerr := c.List(ctx)
	if lerr != nil {
		return 0, fmt.Errorf("uzak dosya boyutu alınamadı: %w", err)
	}
	for _, f := range files {
		if f.Name == name {
			return f.Size, nil
		}
	}
	return 0, fmt.Errorf("uzak dosya bulunamadı: %s", name)
}

func (c *ftpClient) Delete(ctx context.Context, name string) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	if err := c.conn.Delete(full); err != nil {
		return fmt.Errorf("silinemedi: %w", err)
	}
	return nil
}

func (c *ftpClient) Rename(ctx context.Context, from, to string) error {
	src, err := SafeJoin(c.root, from)
	if err != nil {
		return err
	}
	dst, err := SafeJoin(c.root, to)
	if err != nil {
		return err
	}
	if err := c.conn.Rename(src, dst); err != nil {
		// Bazı sunucular hedef varken RNTO'yu reddeder.
		_ = c.conn.Delete(dst)
		if err2 := c.conn.Rename(src, dst); err2 != nil {
			return fmt.Errorf("yeniden adlandırılamadı: %w", err)
		}
	}
	return nil
}

func (c *ftpClient) Close() error {
	return c.conn.Quit()
}
