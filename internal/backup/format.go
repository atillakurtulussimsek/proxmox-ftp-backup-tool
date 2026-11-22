package backup

import (
	"fmt"
	"time"
)

// HumanBytes, bayt değerini okunabilir biçime çevirir (1024 tabanlı).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// HumanDuration, süreyi Türkçe kısa biçimde yazar.
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%d sa %d dk %d sn", h, m, s)
	case m > 0:
		return fmt.Sprintf("%d dk %d sn", m, s)
	default:
		return fmt.Sprintf("%d sn", s)
	}
}
