package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/backup"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/scheduler"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

// ---------------------------------------------------------------- kontrol paneli

type dashboardData struct {
	Last          *store.Job
	ScheduleOn    bool
	Cron          string
	NextRun       time.Time
	FTPConfigured bool
	FTPTarget     string
	SelectedCount int
	DumpDir       string
	FreeSpace     int64
	FreeErr       string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	last, err := s.st.LastJob()
	if err != nil {
		s.log.Error("son iş okunamadı", "err", err)
	}
	d := dashboardData{
		Last:          last,
		ScheduleOn:    cfg.Schedule.Enabled,
		Cron:          cfg.Schedule.Cron,
		NextRun:       s.sched.Next(),
		FTPConfigured: cfg.FTPConfigured(),
		FTPTarget:     ftpTarget(cfg),
		SelectedCount: len(cfg.Backup.VMIDs),
		DumpDir:       cfg.Backup.DumpDir,
	}
	if free, err := backup.FreeSpace(cfg.Backup.DumpDir); err != nil {
		d.FreeErr = err.Error()
	} else {
		d.FreeSpace = free
	}
	s.render(w, r, "dashboard", "Kontrol Paneli", d)
}

func ftpTarget(cfg config.Config) string {
	if !cfg.FTPConfigured() {
		return ""
	}
	return cfg.FTP.Protocol + "://" + cfg.FTP.Username + "@" + cfg.FTP.Host + ":" + strconv.Itoa(cfg.FTP.Port) + cfg.FTP.RemoteDir
}

type usageData struct {
	Err    string
	Total  int64
	Count  int
	Groups []groupView
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	groups, _, total, err := s.loadRemote(r.Context())
	d := usageData{Total: total, Groups: groups}
	if err != nil {
		d.Err = err.Error()
	}
	for _, g := range groups {
		d.Count += len(g.Sets)
	}
	s.fragment(w, "usage", d)
}

type guestRow struct {
	backup.Guest
	Selected bool
	Keep     int
}

type guestsData struct {
	Err         string
	Guests      []guestRow
	DefaultKeep int
}

func (s *Server) guestRows(ctx context.Context) guestsData {
	cfg := s.cfg.Get()
	d := guestsData{DefaultKeep: cfg.Backup.DefaultKeep}
	guests, err := backup.ListGuests(ctx)
	if err != nil {
		d.Err = err.Error()
		return d
	}
	sel := map[int]bool{}
	for _, id := range cfg.Backup.VMIDs {
		sel[id] = true
	}
	for _, g := range guests {
		d.Guests = append(d.Guests, guestRow{Guest: g, Selected: sel[g.VMID], Keep: cfg.Backup.KeepPerVM[g.VMID]})
	}
	return d
}

func (s *Server) handleDashboardGuests(w http.ResponseWriter, r *http.Request) {
	s.fragment(w, "dashboardGuests", s.guestRows(r.Context()))
}

// ---------------------------------------------------------------- iş tetikleme

func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	vmids := cfg.Backup.VMIDs
	if v := r.PostFormValue("vmid"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id <= 0 {
			s.flash(w, false, "Geçersiz VM ID")
			return
		}
		vmids = []int{id}
	}
	id, err := s.runner.StartBackup(store.TRIGGER_MANUAL, vmids)
	if err != nil {
		s.flash(w, false, "Yedekleme başlatılamadı: %v", err)
		return
	}
	w.Header().Set("HX-Trigger", "jobChanged")
	s.flash(w, true, "Yedekleme başlatıldı (iş #%d).", id)
}

func (s *Server) handleRunCleanup(w http.ResponseWriter, r *http.Request) {
	id, err := s.runner.StartCleanup(store.TRIGGER_MANUAL)
	if err != nil {
		s.flash(w, false, "Temizlik başlatılamadı: %v", err)
		return
	}
	w.Header().Set("HX-Trigger", "jobChanged")
	s.flash(w, true, "Saklama temizliği başlatıldı (iş #%d).", id)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if !s.runner.Cancel() {
		s.flash(w, false, "Çalışan iş yok.")
		return
	}
	w.Header().Set("HX-Trigger", "jobChanged")
	s.flash(w, true, "İptal isteği gönderildi.")
}

type statusData struct {
	Running bool
	S       backup.Status
	Logs    []store.LogLine
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.runner.Status()
	d := statusData{Running: ok, S: st}
	if ok {
		logs, err := s.st.TailLogs(st.JobID, 15)
		if err == nil {
			d.Logs = logs
		}
	}
	s.fragment(w, "jobStatus", d)
}

// ---------------------------------------------------------------- geçmiş

const HISTORY_PAGE_SIZE = 25

type historyData struct {
	Jobs  []store.Job
	Page  int
	Pages int
	Total int
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	jobs, total, err := s.st.ListJobs(HISTORY_PAGE_SIZE, (page-1)*HISTORY_PAGE_SIZE)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pages := (total + HISTORY_PAGE_SIZE - 1) / HISTORY_PAGE_SIZE
	s.render(w, r, "history", "Geçmiş", historyData{Jobs: jobs, Page: page, Pages: max(pages, 1), Total: total})
}

type jobData struct {
	Job  *store.Job
	Logs []store.LogLine
}

func (s *Server) loadJob(r *http.Request) (*jobData, int) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	job, err := s.st.GetJob(id)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	if job == nil {
		return nil, http.StatusNotFound
	}
	logs, err := s.st.Logs(id, 0, 50000)
	if err != nil {
		return nil, http.StatusInternalServerError
	}
	return &jobData{Job: job, Logs: logs}, 0
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	d, code := s.loadJob(r)
	if d == nil {
		http.Error(w, http.StatusText(code), code)
		return
	}
	s.render(w, r, "job", "İş #"+strconv.FormatInt(d.Job.ID, 10), d)
}

func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	d, code := s.loadJob(r)
	if d == nil {
		http.Error(w, http.StatusText(code), code)
		return
	}
	s.fragment(w, "jobLog", d)
}

// ---------------------------------------------------------------- cron önizleme

type cronData struct {
	Desc string
	Next []time.Time
	Err  string
}

func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) {
	spec := r.URL.Query().Get("cron")
	d := cronData{Desc: scheduler.Describe(spec)}
	next, err := scheduler.NextRuns(spec, 3)
	if err != nil {
		d.Err = err.Error()
	}
	d.Next = next
	s.fragment(w, "cronPreview", d)
}
