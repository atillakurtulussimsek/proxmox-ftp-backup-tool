package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Guest, yerel node üzerindeki bir VM veya LXC'dir.
type Guest struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Type     string `json:"type"` // qemu | lxc
	Status   string `json:"status"`
	Node     string `json:"node"`
	MaxDisk  int64  `json:"maxdisk"`
	Template int    `json:"template"`
}

// LocalNode, Proxmox node adını döndürür (kısa hostname).
func LocalNode() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// ListGuests, pvesh ile yalnızca yerel node'daki VM/LXC'leri listeler.
func ListGuests(ctx context.Context) ([]Guest, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("pvesh hatası: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("pvesh çalıştırılamadı: %w", err)
	}
	var all []Guest
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("pvesh çıktısı çözümlenemedi: %w", err)
	}
	node := LocalNode()
	var guests []Guest
	for _, g := range all {
		if g.Node != node || g.Template == 1 {
			continue
		}
		guests = append(guests, g)
	}
	sort.Slice(guests, func(i, j int) bool { return guests[i].VMID < guests[j].VMID })
	return guests, nil
}

// DiskSpace, klasörün bulunduğu dosya sistemindeki boş ve toplam alanı döndürür.
func DiskSpace(dir string) (free, total int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), int64(uint64(st.Blocks) * uint64(st.Bsize)), nil
}

// FreeSpace, verilen klasörün bulunduğu dosya sistemindeki boş alanı döndürür.
func FreeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), nil
}
