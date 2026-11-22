package backup

import (
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
)

// STAMP_LAYOUT, vzdump dosya adlarındaki zaman damgası biçimidir.
const STAMP_LAYOUT = "2006_01_02-15_04_05"

var archiveExtRe = regexp.MustCompile(`^(vma|tar)(\.(zst|gz|lzo))?$`)

var nameRe = regexp.MustCompile(`^vzdump-(qemu|lxc)-(\d+)-(\d{4}_\d{2}_\d{2}-\d{2}_\d{2}_\d{2})\.(.+)$`)

// BackupFile, ayrıştırılmış bir vzdump dosyasıdır.
type BackupFile struct {
	Name    string
	Type    string // qemu | lxc
	VMID    int
	Stamp   string
	Time    time.Time
	Ext     string
	Size    int64
	ModTime time.Time
}

// IsArchive, dosyanın geri yüklenebilir yedek arşivi olup olmadığını söyler.
func (f BackupFile) IsArchive() bool {
	return archiveExtRe.MatchString(f.Ext)
}

// ParseName, dosya adını vzdump biçimine göre ayrıştırır. Biçime uymayan adlar için false döner.
func ParseName(name string) (BackupFile, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return BackupFile{}, false
	}
	vmid, err := strconv.Atoi(m[2])
	if err != nil || vmid <= 0 {
		return BackupFile{}, false
	}
	t, err := time.ParseInLocation(STAMP_LAYOUT, m[3], time.Local)
	if err != nil {
		return BackupFile{}, false
	}
	return BackupFile{Name: name, Type: m[1], VMID: vmid, Stamp: m[3], Time: t, Ext: m[4]}, true
}

// BackupSet, aynı VM'in aynı zaman damgalı dosyalarıdır (arşiv, log, notes...).
type BackupSet struct {
	Stamp string
	Time  time.Time
	Files []BackupFile
	Size  int64
}

// VMGroup, bir VM'in tüm yedek setleridir; setler yeniden eskiye sıralıdır.
type VMGroup struct {
	VMID int
	Type string
	Sets []BackupSet
	Size int64
}

// Group, uzak dosyaları VM ve zaman damgasına göre gruplar. Biçime uymayan dosyalar
// ayrı döndürülür ve hiçbir işleme dahil edilmez.
func Group(files []ftpclient.FileInfo) (groups []VMGroup, others []ftpclient.FileInfo) {
	byVM := map[int]map[string]*BackupSet{}
	types := map[int]string{}
	for _, fi := range files {
		bf, ok := ParseName(fi.Name)
		if !ok {
			others = append(others, fi)
			continue
		}
		bf.Size = fi.Size
		bf.ModTime = fi.ModTime
		sets := byVM[bf.VMID]
		if sets == nil {
			sets = map[string]*BackupSet{}
			byVM[bf.VMID] = sets
		}
		s := sets[bf.Stamp]
		if s == nil {
			s = &BackupSet{Stamp: bf.Stamp, Time: bf.Time}
			sets[bf.Stamp] = s
		}
		s.Files = append(s.Files, bf)
		s.Size += bf.Size
		if bf.IsArchive() || types[bf.VMID] == "" {
			types[bf.VMID] = bf.Type
		}
	}
	for vmid, sets := range byVM {
		g := VMGroup{VMID: vmid, Type: types[vmid]}
		for _, s := range sets {
			sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Name < s.Files[j].Name })
			g.Sets = append(g.Sets, *s)
			g.Size += s.Size
		}
		sort.Slice(g.Sets, func(i, j int) bool { return g.Sets[i].Stamp > g.Sets[j].Stamp })
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].VMID < groups[j].VMID })
	sort.Slice(others, func(i, j int) bool { return others[i].Name < others[j].Name })
	return groups, others
}

// PlanRetention, KEEP kuralına göre silinecek dosyaları döndürür. Her VM için en yeni
// keep adet zaman damgası tutulur. skip içindeki VM'lere dokunulmaz. keep < 1 ise 1 kabul edilir.
func PlanRetention(groups []VMGroup, keepFor func(vmid int) int, skip map[int]bool) []BackupFile {
	var del []BackupFile
	for _, g := range groups {
		if skip[g.VMID] {
			continue
		}
		keep := keepFor(g.VMID)
		if keep < 1 {
			keep = 1
		}
		if len(g.Sets) <= keep {
			continue
		}
		for _, s := range g.Sets[keep:] {
			del = append(del, s.Files...)
		}
	}
	return del
}
