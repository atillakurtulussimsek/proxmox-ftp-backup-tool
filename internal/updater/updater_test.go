package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.2.0", "v1.10.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0-rc1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc1", false},
		{"v2.0.0", "v1.9.9", false},
		{"dev", "v0.1.0", true},
		{"0bb9b7a-dirty", "v0.1.0", true},
		{"v1.0.0", "latest", false},
		{"v1.0.0", "v1.0", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.cur, c.latest); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, beklenen %v", c.cur, c.latest, got, c.want)
		}
	}
}

// fakeBinary, "version" ile çağrılınca tag yazan bir kabuk betiğidir.
func fakeBinary(tag string) []byte {
	return []byte("#!/bin/sh\necho " + tag + "\n")
}

func newServer(t *testing.T, tag string, bin []byte, sum string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+REPO+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":"x","assets":[{"name":%q,"browser_download_url":%q},{"name":%q,"browser_download_url":%q}]}`,
			tag, ASSET_BINARY, srv.URL+"/bin", ASSET_SUMS, srv.URL+"/sums")
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) { w.Write(bin) })
	mux.HandleFunc("/sums", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  other-file.tar.gz\n%s  %s\n", strings.Repeat("0", 64), sum, ASSET_BINARY)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := API_BASE
	API_BASE = srv.URL
	t.Cleanup(func() { API_BASE = old })
	return srv
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestApply(t *testing.T) {
	bin := fakeBinary("v9.9.9")
	newServer(t, "v9.9.9", bin, sha(bin))
	exe := filepath.Join(t.TempDir(), "pve-ftp-backup")
	os.WriteFile(exe, []byte("old"), 0o755)

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rel.Installable() {
		t.Fatal("kurulabilir olmalı")
	}
	if err := Apply(context.Background(), rel, exe); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(bin) {
		t.Fatal("binary değişmedi")
	}
}

func TestApplyRejects(t *testing.T) {
	cases := map[string]struct {
		bin []byte
		sum string
	}{
		"bozuk özet":       {fakeBinary("v9.9.9"), sha([]byte("başka"))},
		"yanlış sürüm":     {fakeBinary("v1.0.0"), sha(fakeBinary("v1.0.0"))},
		"çalışmayan dosya": {[]byte("not a binary"), sha([]byte("not a binary"))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			newServer(t, "v9.9.9", c.bin, c.sum)
			dir := t.TempDir()
			exe := filepath.Join(dir, "pve-ftp-backup")
			os.WriteFile(exe, []byte("old"), 0o755)
			rel, err := Latest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(context.Background(), rel, exe); err == nil {
				t.Fatal("hata bekleniyordu")
			}
			got, _ := os.ReadFile(exe)
			if string(got) != "old" {
				t.Fatal("eski binary korunmalı")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("geçici dosya kaldı: %d dosya", len(entries))
			}
		})
	}
}
