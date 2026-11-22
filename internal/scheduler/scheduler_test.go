package scheduler

import "testing"

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"0 2 * * *":    "Her gün 02:00",
		"30 1 * * 1-5": "Hafta içi her gün 01:30",
		"0 3 * * 0":    "Her Pazar 03:00",
		"0 3 * * 1,4":  "Her Pazartesi, Perşembe 03:00",
		"15 4 1 * *":   "Her ayın 1. günü 04:15",
		"0 2,14 * * *": "Her gün 02:00, 14:00",
		"0 */6 * * *":  "6 saatte bir, :00 dakikada",
		"@daily":       "Her gün 00:00",
		"0 2 * 1 *":    "Özel zamanlama",
		"bozuk":        "Özel zamanlama",
	}
	for in, want := range cases {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%q) = %q, beklenen %q", in, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"0 2 * * *", "*/15 * * * *", "@weekly"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("%q geçerli olmalı: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "0 2 * *", "61 2 * * *", "@every 1h", "0 0 2 * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q geçersiz olmalı", bad)
		}
	}
}
