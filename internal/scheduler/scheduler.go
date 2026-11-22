// Package scheduler, cron ifadesine göre zamanlanmış yedeklemeyi uygulama içinde çalıştırır.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Parse, 5 alanlı cron ifadesini (veya @daily gibi tanımlayıcıları) çözümler.
func Parse(spec string) (cron.Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("cron ifadesi boş")
	}
	if strings.HasPrefix(spec, "@every") {
		return nil, fmt.Errorf("@every desteklenmiyor")
	}
	s, err := parser.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("geçersiz cron ifadesi: %v", err)
	}
	return s, nil
}

// NextRuns, ifadenin sonraki n çalışma zamanını döndürür.
func NextRuns(spec string, n int) ([]time.Time, error) {
	s, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, n)
	t := time.Now()
	for i := 0; i < n; i++ {
		t = s.Next(t)
		if t.IsZero() {
			break
		}
		out = append(out, t)
	}
	return out, nil
}

type Scheduler struct {
	mu      sync.Mutex
	c       *cron.Cron
	entry   cron.EntryID
	run     func()
	enabled bool
}

// New, zamanlanmış her tetiklemede run fonksiyonunu çağıran bir zamanlayıcı oluşturur.
func New(run func()) *Scheduler {
	c := cron.New(cron.WithParser(parser), cron.WithLocation(time.Local))
	c.Start()
	return &Scheduler{c: c, run: run}
}

// Reload, zamanlamayı yeni ifadeyle günceller.
func (s *Scheduler) Reload(enabled bool, spec string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entry != 0 {
		s.c.Remove(s.entry)
		s.entry = 0
	}
	s.enabled = false
	if !enabled {
		return nil
	}
	sched, err := Parse(spec)
	if err != nil {
		return err
	}
	s.entry = s.c.Schedule(sched, cron.FuncJob(s.run))
	s.enabled = true
	return nil
}

// Next, sonraki zamanlanmış çalışmayı döndürür; zamanlama kapalıysa sıfır zaman.
func (s *Scheduler) Next() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || s.entry == 0 {
		return time.Time{}
	}
	return s.c.Entry(s.entry).Next
}

func (s *Scheduler) Stop() {
	<-s.c.Stop().Done()
}

var dayNames = []string{"Pazar", "Pazartesi", "Salı", "Çarşamba", "Perşembe", "Cuma", "Cumartesi"}

// Describe, cron ifadesini okunabilir Türkçe metne çevirir. Tanınmayan kalıplar için
// genel bir açıklama döndürür.
func Describe(spec string) string {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "@yearly", "@annually":
		return "Her yıl 1 Ocak 00:00"
	case "@monthly":
		return "Her ayın 1'i 00:00"
	case "@weekly":
		return "Her Pazar 00:00"
	case "@daily", "@midnight":
		return "Her gün 00:00"
	case "@hourly":
		return "Her saat başı"
	}
	f := strings.Fields(spec)
	if len(f) != 5 {
		return "Özel zamanlama"
	}
	min, hour, dom, mon, dow := f[0], f[1], f[2], f[3], f[4]
	mi, errM := strconv.Atoi(min)
	if mon != "*" {
		return "Özel zamanlama"
	}
	if errM == nil && strings.HasPrefix(hour, "*/") && dom == "*" && dow == "*" {
		return fmt.Sprintf("%s saatte bir, :%02d dakikada", strings.TrimPrefix(hour, "*/"), mi)
	}
	if errM == nil && hour == "*" && dom == "*" && dow == "*" {
		return fmt.Sprintf("Her saat :%02d dakikada", mi)
	}
	if strings.HasPrefix(min, "*/") && hour == "*" && dom == "*" && dow == "*" {
		return fmt.Sprintf("%s dakikada bir", strings.TrimPrefix(min, "*/"))
	}
	times := clockTimes(min, hour)
	if times == "" {
		return "Özel zamanlama"
	}
	switch {
	case dom == "*" && dow == "*":
		return "Her gün " + times
	case dom == "*":
		days, ok := describeDow(dow)
		if !ok {
			return "Özel zamanlama"
		}
		return days + " " + times
	case dow == "*":
		if _, err := strconv.Atoi(strings.ReplaceAll(dom, ",", "")); err != nil {
			return "Özel zamanlama"
		}
		return "Her ayın " + strings.ReplaceAll(dom, ",", ", ") + ". günü " + times
	}
	return "Özel zamanlama"
}

func clockTimes(min, hour string) string {
	mi, err := strconv.Atoi(min)
	if err != nil || mi < 0 || mi > 59 {
		return ""
	}
	var parts []string
	for _, h := range strings.Split(hour, ",") {
		hi, err := strconv.Atoi(h)
		if err != nil || hi < 0 || hi > 23 {
			return ""
		}
		parts = append(parts, fmt.Sprintf("%02d:%02d", hi, mi))
	}
	return strings.Join(parts, ", ")
}

func describeDow(dow string) (string, bool) {
	switch dow {
	case "1-5", "MON-FRI", "mon-fri":
		return "Hafta içi her gün", true
	case "0,6", "6,0", "SAT,SUN", "sat,sun":
		return "Hafta sonu her gün", true
	}
	var names []string
	for _, d := range strings.Split(dow, ",") {
		i, err := strconv.Atoi(d)
		if err != nil || i < 0 || i > 7 {
			return "", false
		}
		names = append(names, dayNames[i%7])
	}
	return "Her " + strings.Join(names, ", "), true
}
