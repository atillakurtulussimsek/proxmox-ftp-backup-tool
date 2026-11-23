package backup

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
)

// Uzak klasör düzeni (kök = ayarlardaki uzak klasör):
//
//	<kök>/backup/<vmid>/vzdump-...vma.zst   arşiv ve diğer vzdump dosyaları
//	<kök>/logs/<vmid>/vzdump-...log         vzdump log dosyaları
const (
	REMOTE_BACKUP_DIR = "backup"
	REMOTE_LOGS_DIR   = "logs"
)

// RemotePath, vzdump dosya adının uzak sunucudaki köke göre göreli yolunu döndürür.
func RemotePath(name string) (string, error) {
	bf, ok := ParseName(name)
	if !ok {
		return "", fmt.Errorf("%w: vzdump dosyası değil: %q", ftpclient.ErrInvalidName, name)
	}
	if err := ftpclient.ValidateName(name); err != nil {
		return "", err
	}
	dir := REMOTE_BACKUP_DIR
	if bf.Ext == "log" {
		dir = REMOTE_LOGS_DIR
	}
	return path.Join(dir, strconv.Itoa(bf.VMID), name), nil
}

// ListRemote, backup/<vmid> ve logs/<vmid> klasörlerini tarar. files, doğru yerde duran
// vzdump dosyalarıdır (Name = dosya adı). others, düzene uymayan diğer her şeydir
// (Name = köke göre göreli yol); bunlara hiçbir zaman dokunulmaz. Kökteki dosyalar da
// others'a eklenir.
func ListRemote(ctx context.Context, c ftpclient.Client) (files, others []ftpclient.FileInfo, err error) {
	root, err := c.List(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	for _, top := range root {
		if !top.IsDir {
			others = append(others, top)
			continue
		}
		if top.Name != REMOTE_BACKUP_DIR && top.Name != REMOTE_LOGS_DIR {
			continue
		}
		vmDirs, err := c.List(ctx, top.Name)
		if err != nil {
			return nil, nil, err
		}
		for _, vd := range vmDirs {
			rel := path.Join(top.Name, vd.Name)
			if !vd.IsDir {
				vd.Name = rel
				others = append(others, vd)
				continue
			}
			if id, err := strconv.Atoi(vd.Name); err != nil || id <= 0 {
				continue
			}
			entries, err := c.List(ctx, rel)
			if err != nil {
				return nil, nil, err
			}
			for _, f := range entries {
				if f.IsDir {
					continue
				}
				if want, err := RemotePath(f.Name); err == nil && want == path.Join(rel, f.Name) {
					files = append(files, f)
					continue
				}
				f.Name = path.Join(rel, f.Name)
				others = append(others, f)
			}
		}
	}
	return files, others, nil
}
