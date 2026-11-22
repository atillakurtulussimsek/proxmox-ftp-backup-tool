package ftpclient

import (
	"errors"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	cases := []struct {
		root, name, want string
		wantErr          bool
	}{
		{"/backups", "vzdump-qemu-100-2024_01_01-00_00_00.vma.zst", "/backups/vzdump-qemu-100-2024_01_01-00_00_00.vma.zst", false},
		{"/backups/", "a.log", "/backups/a.log", false},
		{"", "a.log", "/a.log", false},
		{"/", "a.log", "/a.log", false},
		{"rel/dir", "a.log", "rel/dir/a.log", false},
		{"/backups", "../etc/passwd", "", true},
		{"/backups", "..", "", true},
		{"/backups", ".", "", true},
		{"/backups", "", "", true},
		{"/backups", "sub/a.log", "", true},
		{"/backups", "..\\a", "", true},
		{"/backups", "a\x00b", "", true},
		{"/backups", "a\nb", "", true},
	}
	for _, c := range cases {
		got, err := SafeJoin(c.root, c.name)
		if c.wantErr {
			if err == nil || !errors.Is(err, ErrInvalidName) {
				t.Errorf("SafeJoin(%q,%q) hata bekleniyordu, %q döndü", c.root, c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SafeJoin(%q,%q) beklenmeyen hata: %v", c.root, c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("SafeJoin(%q,%q) = %q, beklenen %q", c.root, c.name, got, c.want)
		}
	}
}
