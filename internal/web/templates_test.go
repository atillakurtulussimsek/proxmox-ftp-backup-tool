package web

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

// Şablon tip hataları ancak çalıştırınca ortaya çıktığı için parçalar örnek veriyle çalıştırılır.
func TestFragments(t *testing.T) {
	base, err := template.New("").Funcs(funcMap()).ParseFS(assets, "assets/templates/layout.html", "assets/templates/partials.html")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cases := map[string]any{
		"jobStatus": statusData{Running: true, S: backup.Status{
			JobID: 1, Kind: store.KIND_BACKUP, Started: now, VMIndex: 1, VMCount: 2, VMID: 100, VMName: "web",
			Transfer: &backup.Transfer{Name: "x", Done: 10, Total: 100, Started: now.Add(-time.Second), Limit: 1_000_000},
		}, Logs: []store.LogLine{{Time: now, Level: "info", Msg: "m"}}},
		"vmTable": vmTableData{FTPOn: true, Rows: []vmRow{{
			Guest: backup.Guest{VMID: 100, Name: "web", Type: "qemu", Status: "running", MaxDisk: 1 << 30},
			Selected: true, Keep: 3, Sets: 2, Size: 10, Newest: now, Slots: []bool{true, true, false}, Fresh: "ok",
			Last: &store.VMLast{JobID: 1, Status: store.STATUS_FAILED, Message: "hata", At: now},
		}}},
		"flash": flash{OK: true, Msg: "tamam"},
	}
	for name, data := range cases {
		var buf bytes.Buffer
		if err := base.ExecuteTemplate(&buf, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	var buf bytes.Buffer
	_ = base.ExecuteTemplate(&buf, "jobStatus", cases["jobStatus"])
	if !strings.Contains(buf.String(), "sınır 8.0 Mbit/sn") {
		t.Errorf("hız sınırı satırı yok: %s", buf.String())
	}
}
