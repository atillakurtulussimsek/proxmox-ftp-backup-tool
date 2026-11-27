package backup

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

// WINDOW_SLACK, iş zaman aralığına iki yönde eklenen paydır (saat farkı, yavaş disk vb.).
const WINDOW_SLACK = 2 * time.Minute

// RESTORE_PREFIX, geri alma sırasında yarım indirmenin geçici adının önekidir.
const RESTORE_PREFIX = ".restore-"

// Leftover, geçici klasörde bu uygulamanın bıraktığı bir dosya ya da klasördür.
type Leftover struct {
	Path  string
	Size  int64
	IsDir bool
}

func inWindow(t time.Time, w store.Window) bool {
	return !t.Before(w.Start.Add(-WINDOW_SLACK)) && !t.After(w.End.Add(WINDOW_SLACK))
}

// FindLeftovers, geçici klasörde bu uygulamanın önceki işlerinden kalanları bulur:
//
//   - vzdump dosya/klasörleri: adındaki zaman damgası VE son değişiklik zamanı aynı
//     yedekleme işinin süresine denk geliyorsa. "Proxmox'a geri al" ile indirilen arşivlerin
//     değişiklik zamanı indirme anıdır; bu yüzden bu koşula uymazlar ve korunurlar.
//   - ".restore-vzdump-..." ile başlayan yarım kalmış geri alma indirmeleri.
//
// Elle konan ya da Proxmox'un kendi yedekleme işlerinin oluşturduğu dosyalara dokunulmaz.
func FindLeftovers(dumpDir string, windows []store.Window) ([]Leftover, error) {
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		return nil, err
	}
	var out []Leftover
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			continue
		}
		mode := info.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			continue // sembolik bağlantı vb.
		}
		ours := false
		if rest, ok := strings.CutPrefix(name, RESTORE_PREFIX); ok {
			_, ours = ParseName(rest)
		} else if bf, ok := ParseName(name); ok {
			for _, w := range windows {
				if inWindow(bf.Time, w) && inWindow(info.ModTime(), w) {
					ours = true
					break
				}
			}
		}
		if !ours {
			continue
		}
		p := filepath.Join(dumpDir, name)
		size := info.Size()
		if mode.IsDir() {
			size = dirSize(p)
		}
		out = append(out, Leftover{Path: p, Size: size, IsDir: mode.IsDir()})
	}
	return out, nil
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// RemoveLeftover, bulunan kalıntıyı siler.
func RemoveLeftover(l Leftover) error {
	if l.IsDir {
		return os.RemoveAll(l.Path)
	}
	return os.Remove(l.Path)
}
