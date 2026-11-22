package backup

import (
	"sort"
	"testing"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
)

func TestParseName(t *testing.T) {
	cases := []struct {
		name    string
		ok      bool
		typ     string
		vmid    int
		stamp   string
		ext     string
		archive bool
	}{
		{"vzdump-qemu-100-2024_05_01-02_00_03.vma.zst", true, "qemu", 100, "2024_05_01-02_00_03", "vma.zst", true},
		{"vzdump-lxc-101-2024_05_01-02_10_00.tar.gz", true, "lxc", 101, "2024_05_01-02_10_00", "tar.gz", true},
		{"vzdump-qemu-100-2024_05_01-02_00_03.log", true, "qemu", 100, "2024_05_01-02_00_03", "log", false},
		{"vzdump-qemu-100-2024_05_01-02_00_03.vma.zst.notes", true, "qemu", 100, "2024_05_01-02_00_03", "vma.zst.notes", false},
		{"vzdump-qemu-100-2024_13_01-02_00_03.vma.zst", false, "", 0, "", "", false},
		{"vzdump-openvz-100-2024_05_01-02_00_03.tar", false, "", 0, "", "", false},
		{"vzdump-qemu-abc-2024_05_01-02_00_03.vma", false, "", 0, "", "", false},
		{"vzdump-qemu-100-2024_05_01-02_00_03", false, "", 0, "", "", false},
		{"my-vzdump-qemu-100-2024_05_01-02_00_03.vma", false, "", 0, "", "", false},
		{"notes.txt", false, "", 0, "", "", false},
		{"vzdump-qemu-0-2024_05_01-02_00_03.vma", false, "", 0, "", "", false},
	}
	for _, c := range cases {
		bf, ok := ParseName(c.name)
		if ok != c.ok {
			t.Errorf("%s: ok=%v beklenen %v", c.name, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if bf.Type != c.typ || bf.VMID != c.vmid || bf.Stamp != c.stamp || bf.Ext != c.ext {
			t.Errorf("%s: %+v", c.name, bf)
		}
		if bf.IsArchive() != c.archive {
			t.Errorf("%s: IsArchive=%v beklenen %v", c.name, bf.IsArchive(), c.archive)
		}
		want := time.Date(bf.Time.Year(), bf.Time.Month(), bf.Time.Day(), bf.Time.Hour(), bf.Time.Minute(), bf.Time.Second(), 0, time.Local)
		if !bf.Time.Equal(want) || bf.Time.Format(STAMP_LAYOUT) != c.stamp {
			t.Errorf("%s: zaman yanlış %v", c.name, bf.Time)
		}
	}
}

func files(names ...string) []ftpclient.FileInfo {
	out := make([]ftpclient.FileInfo, len(names))
	for i, n := range names {
		out[i] = ftpclient.FileInfo{Name: n, Size: 10}
	}
	return out
}

func TestGroup(t *testing.T) {
	in := files(
		"vzdump-qemu-100-2024_05_01-02_00_00.vma.zst",
		"vzdump-qemu-100-2024_05_01-02_00_00.log",
		"vzdump-qemu-100-2024_05_02-02_00_00.vma.zst",
		"vzdump-qemu-100-2024_05_02-02_00_00.log",
		"vzdump-lxc-101-2024_05_02-02_05_00.tar.zst",
		"readme.txt",
		"vzdump-qemu-100-bad.vma",
	)
	groups, others := Group(in)
	if len(groups) != 2 {
		t.Fatalf("2 grup bekleniyordu, %d", len(groups))
	}
	if len(others) != 2 {
		t.Fatalf("2 diğer dosya bekleniyordu, %d", len(others))
	}
	g := groups[0]
	if g.VMID != 100 || g.Type != "qemu" || len(g.Sets) != 2 || g.Size != 40 {
		t.Fatalf("grup 100 hatalı: %+v", g)
	}
	if g.Sets[0].Stamp != "2024_05_02-02_00_00" {
		t.Errorf("setler yeniden eskiye sıralı olmalı: %s", g.Sets[0].Stamp)
	}
	if len(g.Sets[0].Files) != 2 || g.Sets[0].Size != 20 {
		t.Errorf("set dosyaları hatalı: %+v", g.Sets[0])
	}
	if groups[1].VMID != 101 || groups[1].Type != "lxc" {
		t.Errorf("grup 101 hatalı: %+v", groups[1])
	}
}

func names(fs []BackupFile) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	sort.Strings(out)
	return out
}

func TestPlanRetention(t *testing.T) {
	var in []string
	for _, d := range []string{"01", "02", "03", "04", "05"} {
		in = append(in,
			"vzdump-qemu-100-2024_05_"+d+"-02_00_00.vma.zst",
			"vzdump-qemu-100-2024_05_"+d+"-02_00_00.log",
		)
	}
	for _, d := range []string{"01", "02", "03"} {
		in = append(in, "vzdump-lxc-101-2024_05_"+d+"-03_00_00.tar.zst")
	}
	in = append(in, "vzdump-qemu-102-2024_05_01-02_00_00.vma.zst", "random-file.bin", "vzdump-qemu-100-old.vma")
	groups, _ := Group(files(in...))

	keep := map[int]int{100: 3, 101: 1}
	keepFor := func(id int) int {
		if v, ok := keep[id]; ok {
			return v
		}
		return 7
	}

	got := names(PlanRetention(groups, keepFor, nil))
	want := []string{
		"vzdump-lxc-101-2024_05_01-03_00_00.tar.zst",
		"vzdump-lxc-101-2024_05_02-03_00_00.tar.zst",
		"vzdump-qemu-100-2024_05_01-02_00_00.log",
		"vzdump-qemu-100-2024_05_01-02_00_00.vma.zst",
		"vzdump-qemu-100-2024_05_02-02_00_00.log",
		"vzdump-qemu-100-2024_05_02-02_00_00.vma.zst",
	}
	if len(got) != len(want) {
		t.Fatalf("silinecekler:\n%v\nbeklenen:\n%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("silinecekler:\n%v\nbeklenen:\n%v", got, want)
		}
	}

	// skip: 100 atlanır.
	got = names(PlanRetention(groups, keepFor, map[int]bool{100: true}))
	if len(got) != 2 {
		t.Errorf("skip ile 2 dosya bekleniyordu: %v", got)
	}

	// keep 0 → 1 kabul edilir, hiçbir zaman tüm yedekler silinmez.
	got = names(PlanRetention(groups, func(int) int { return 0 }, nil))
	for _, n := range got {
		if n == "vzdump-qemu-100-2024_05_05-02_00_00.vma.zst" || n == "vzdump-lxc-101-2024_05_03-03_00_00.tar.zst" || n == "vzdump-qemu-102-2024_05_01-02_00_00.vma.zst" {
			t.Errorf("en yeni yedek silinmemeli: %s", n)
		}
		if n == "random-file.bin" || n == "vzdump-qemu-100-old.vma" {
			t.Errorf("biçime uymayan dosyaya dokunulmamalı: %s", n)
		}
	}
}
