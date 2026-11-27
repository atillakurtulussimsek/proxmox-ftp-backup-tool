package backup

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

func TestFindLeftovers(t *testing.T) {
	dir := t.TempDir()
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	// Uygulamanın 25.11 02:00–03:00 arasında çalışmış bir yedekleme işi.
	windows := []store.Window{{Start: at("2026-11-25 02:00"), End: at("2026-11-25 03:00")}}

	mk := func(name string, mtime time.Time, isDir bool) {
		p := filepath.Join(dir, name)
		if isDir {
			os.MkdirAll(filepath.Join(p, "sub"), 0o755)
			os.WriteFile(filepath.Join(p, "sub", "x"), make([]byte, 100), 0o644)
		} else {
			os.WriteFile(p, make([]byte, 10), 0o644)
		}
		os.Chtimes(p, mtime, mtime)
	}
	// Bizim: iş sırasında oluşan arşiv, log ve vzdump geçici klasörü.
	mk("vzdump-qemu-101-2026_11_25-02_10_00.vma.zst", at("2026-11-25 02:40"), false)
	mk("vzdump-qemu-101-2026_11_25-02_10_00.log", at("2026-11-25 02:41"), false)
	mk("vzdump-lxc-107-2026_11_25-02_50_00.tmp", at("2026-11-25 02:55"), true)
	// Bizim: yarım kalmış geri alma indirmesi.
	mk(".restore-vzdump-qemu-101-2026_11_20-02_00_00.vma.zst", at("2026-11-27 10:00"), false)
	// Bizim değil: aynı yedeğin sonradan geri alınmış hali (değişiklik zamanı farklı).
	mk("vzdump-qemu-102-2026_11_25-02_20_00.vma.zst", at("2026-11-27 10:00"), false)
	// Bizim değil: iş dışında oluşmuş (Proxmox'un kendi işi / elle).
	mk("vzdump-qemu-103-2026_11_26-22_00_00.vma.zst", at("2026-11-26 22:30"), false)
	// Bizim değil: vzdump biçiminde olmayan.
	mk("notlar.txt", at("2026-11-25 02:30"), false)

	got, err := FindLeftovers(dir, windows)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range got {
		names = append(names, filepath.Base(l.Path))
	}
	sort.Strings(names)
	want := []string{
		".restore-vzdump-qemu-101-2026_11_20-02_00_00.vma.zst",
		"vzdump-lxc-107-2026_11_25-02_50_00.tmp",
		"vzdump-qemu-101-2026_11_25-02_10_00.log",
		"vzdump-qemu-101-2026_11_25-02_10_00.vma.zst",
	}
	if len(names) != len(want) {
		t.Fatalf("bulunan %v, beklenen %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("bulunan %v, beklenen %v", names, want)
		}
	}
	for _, l := range got {
		if l.IsDir && l.Size != 100 {
			t.Errorf("klasör boyutu %d, beklenen 100", l.Size)
		}
		if err := RemoveLeftover(l); err != nil {
			t.Fatal(err)
		}
	}
	rest, _ := os.ReadDir(dir)
	if len(rest) != 3 {
		t.Fatalf("silinmemesi gereken 3 öğe kalmalı, kalan %d", len(rest))
	}
}
