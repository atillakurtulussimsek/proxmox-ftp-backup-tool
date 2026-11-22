package ftpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
)

type sftpClient struct {
	ssh     *ssh.Client
	sftp    *sftp.Client
	root    string
	hostKey string
}

func dialSFTP(ctx context.Context, cfg config.FTPConfig) (*sftpClient, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var seen string
	sshCfg := &ssh.ClientConfig{
		User: cfg.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = cfg.Password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if cfg.HostKey != "" && cfg.HostKey != seen {
				return fmt.Errorf("sunucu anahtarı değişmiş! kayıtlı: %s, gelen: %s", cfg.HostKey, seen)
			}
			return nil
		},
		Timeout: 30 * time.Second,
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	netConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("sunucuya bağlanılamadı: %w", err)
	}
	c, chans, reqs, err := ssh.NewClientConn(netConn, addr, sshCfg)
	if err != nil {
		netConn.Close()
		return nil, fmt.Errorf("SSH oturumu açılamadı: %w", err)
	}
	sshClient := ssh.NewClient(c, chans, reqs)
	sc, err := sftp.NewClient(sshClient, sftp.UseConcurrentWrites(true))
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("SFTP oturumu açılamadı: %w", err)
	}
	return &sftpClient{ssh: sshClient, sftp: sc, root: CleanRoot(cfg.RemoteDir), hostKey: seen}, nil
}

func (c *sftpClient) HostKey() string { return c.hostKey }

func (c *sftpClient) EnsureRoot(ctx context.Context) error {
	if err := c.sftp.MkdirAll(c.root); err != nil {
		return fmt.Errorf("uzak klasör oluşturulamadı: %w", err)
	}
	return nil
}

func (c *sftpClient) List(ctx context.Context) ([]FileInfo, error) {
	entries, err := c.sftp.ReadDir(c.root)
	if err != nil {
		return nil, fmt.Errorf("uzak klasör listelenemedi: %w", err)
	}
	out := make([]FileInfo, 0, len(entries))
	for _, e := range entries {
		if !e.Mode().IsRegular() || ValidateName(e.Name()) != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Size: e.Size(), ModTime: e.ModTime()})
	}
	return out, nil
}

func (c *sftpClient) Upload(ctx context.Context, localPath, name string, progress ProgressFunc) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	dst, err := c.sftp.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("uzak dosya oluşturulamadı: %w", err)
	}
	_, cerr := dst.ReadFrom(&ctxReader{ctx: ctx, r: f, progress: progress})
	closeErr := dst.Close()
	if cerr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("yükleme başarısız: %w", cerr)
	}
	return closeErr
}

func (c *sftpClient) Download(ctx context.Context, name string, w io.Writer, progress ProgressFunc) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	src, err := c.sftp.Open(full)
	if err != nil {
		return fmt.Errorf("uzak dosya açılamadı: %w", err)
	}
	defer src.Close()
	_, err = copyCtx(ctx, w, src, progress)
	return err
}

func (c *sftpClient) Size(ctx context.Context, name string) (int64, error) {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return 0, err
	}
	st, err := c.sftp.Stat(full)
	if err != nil {
		return 0, fmt.Errorf("uzak dosya boyutu alınamadı: %w", err)
	}
	return st.Size(), nil
}

func (c *sftpClient) Delete(ctx context.Context, name string) error {
	full, err := SafeJoin(c.root, name)
	if err != nil {
		return err
	}
	if err := c.sftp.Remove(full); err != nil {
		return fmt.Errorf("silinemedi: %w", err)
	}
	return nil
}

func (c *sftpClient) Rename(ctx context.Context, from, to string) error {
	src, err := SafeJoin(c.root, from)
	if err != nil {
		return err
	}
	dst, err := SafeJoin(c.root, to)
	if err != nil {
		return err
	}
	if err := c.sftp.PosixRename(src, dst); err == nil {
		return nil
	}
	_ = c.sftp.Remove(dst)
	if err := c.sftp.Rename(src, dst); err != nil {
		return fmt.Errorf("yeniden adlandırılamadı: %w", err)
	}
	return nil
}

func (c *sftpClient) Close() error {
	return errors.Join(c.sftp.Close(), c.ssh.Close())
}
