// Package store, iş geçmişi ve logları SQLite veritabanında saklar.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// İş türleri.
const (
	KIND_BACKUP  = "backup"
	KIND_CLEANUP = "cleanup"
	KIND_RESTORE = "restore"
)

// Tetikleyiciler.
const (
	TRIGGER_SCHEDULED = "scheduled"
	TRIGGER_MANUAL    = "manual"
)

// Durumlar.
const (
	STATUS_RUNNING   = "running"
	STATUS_SUCCESS   = "success"
	STATUS_FAILED    = "failed"
	STATUS_PARTIAL   = "partial"
	STATUS_CANCELLED = "cancelled"
	STATUS_SKIPPED   = "skipped"
	STATUS_REJECTED  = "rejected"
)

type Job struct {
	ID            int64
	Kind          string
	Trigger       string
	Status        string
	StartedAt     time.Time
	FinishedAt    time.Time
	Summary       string
	BytesUploaded int64
	VMs           []VMResult
}

// Duration, işin süresini döndürür. Bitmemişse şu ana kadar geçen süre.
func (j Job) Duration() time.Duration {
	if j.FinishedAt.IsZero() {
		return time.Since(j.StartedAt).Round(time.Second)
	}
	return j.FinishedAt.Sub(j.StartedAt).Round(time.Second)
}

type VMResult struct {
	VMID     int
	Name     string
	Status   string
	Message  string
	Size     int64
	Duration time.Duration
}

type LogLine struct {
	ID    int64
	Time  time.Time
	Level string
	Msg   string
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	trigger TEXT NOT NULL,
	status TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	summary TEXT NOT NULL DEFAULT '',
	bytes_uploaded INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_jobs_started ON jobs(started_at);
CREATE TABLE IF NOT EXISTS job_vms (
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	vmid INTEGER NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_job_vms_job ON job_vms(job_id);
CREATE TABLE IF NOT EXISTS job_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	level TEXT NOT NULL,
	msg TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_job_logs_job ON job_logs(job_id, id);
`

// Open, veritabanını açar ve şemayı oluşturur.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("veri klasörü oluşturulamadı: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("veritabanı şeması oluşturulamadı: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// MarkInterrupted, uygulama kapanırken yarım kalan işleri hatalı olarak işaretler.
func (s *Store) MarkInterrupted() error {
	_, err := s.db.Exec(`UPDATE jobs SET status=?, finished_at=?, summary=CASE WHEN summary='' THEN 'Uygulama yeniden başlatıldığı için iş yarıda kaldı.' ELSE summary END WHERE status=?`,
		STATUS_FAILED, time.Now().Unix(), STATUS_RUNNING)
	return err
}

func (s *Store) CreateJob(kind, trigger string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO jobs(kind, trigger, status, started_at) VALUES(?,?,?,?)`,
		kind, trigger, STATUS_RUNNING, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RecordRejected, kilit nedeniyle reddedilen bir tetiklemeyi kaydeder.
func (s *Store) RecordRejected(kind, trigger, msg string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`INSERT INTO jobs(kind, trigger, status, started_at, finished_at, summary) VALUES(?,?,?,?,?,?)`,
		kind, trigger, STATUS_REJECTED, now, now, msg)
	return err
}

func (s *Store) FinishJob(id int64, status, summary string, bytesUploaded int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET status=?, finished_at=?, summary=?, bytes_uploaded=? WHERE id=?`,
		status, time.Now().Unix(), summary, bytesUploaded, id)
	return err
}

func (s *Store) AddVMResult(jobID int64, r VMResult) error {
	_, err := s.db.Exec(`INSERT INTO job_vms(job_id, vmid, name, status, message, size, duration_ms) VALUES(?,?,?,?,?,?,?)`,
		jobID, r.VMID, r.Name, r.Status, r.Message, r.Size, r.Duration.Milliseconds())
	return err
}

func (s *Store) AppendLog(jobID int64, level, msg string) error {
	_, err := s.db.Exec(`INSERT INTO job_logs(job_id, ts, level, msg) VALUES(?,?,?,?)`,
		jobID, time.Now().UnixMilli(), level, msg)
	return err
}

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var started, finished int64
	if err := sc.Scan(&j.ID, &j.Kind, &j.Trigger, &j.Status, &started, &finished, &j.Summary, &j.BytesUploaded); err != nil {
		return j, err
	}
	j.StartedAt = time.Unix(started, 0)
	if finished > 0 {
		j.FinishedAt = time.Unix(finished, 0)
	}
	return j, nil
}

const jobCols = `id, kind, trigger, status, started_at, finished_at, summary, bytes_uploaded`

// ListJobs, işleri yeniden eskiye listeler ve toplam kayıt sayısını döndürür.
func (s *Store) ListJobs(limit, offset int) ([]Job, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for i := range jobs {
		vms, err := s.vmResults(jobs[i].ID)
		if err != nil {
			return nil, 0, err
		}
		jobs[i].VMs = vms
	}
	return jobs, total, nil
}

// LastJob, reddedilenler hariç en son işi döndürür.
func (s *Store) LastJob() (*Job, error) {
	row := s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE status<>? ORDER BY id DESC LIMIT 1`, STATUS_REJECTED)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.VMs, err = s.vmResults(j.ID)
	return &j, err
}

func (s *Store) GetJob(id int64) (*Job, error) {
	row := s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.VMs, err = s.vmResults(j.ID)
	return &j, err
}

func (s *Store) vmResults(jobID int64) ([]VMResult, error) {
	rows, err := s.db.Query(`SELECT vmid, name, status, message, size, duration_ms FROM job_vms WHERE job_id=? ORDER BY rowid`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VMResult
	for rows.Next() {
		var r VMResult
		var ms int64
		if err := rows.Scan(&r.VMID, &r.Name, &r.Status, &r.Message, &r.Size, &ms); err != nil {
			return nil, err
		}
		r.Duration = time.Duration(ms) * time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// Logs, bir işin afterID'den sonraki log satırlarını döndürür.
func (s *Store) Logs(jobID, afterID int64, limit int) ([]LogLine, error) {
	rows, err := s.db.Query(`SELECT id, ts, level, msg FROM job_logs WHERE job_id=? AND id>? ORDER BY id LIMIT ?`, jobID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLogs(rows)
}

// TailLogs, bir işin son n log satırını eskiden yeniye döndürür.
func (s *Store) TailLogs(jobID int64, n int) ([]LogLine, error) {
	rows, err := s.db.Query(`SELECT id, ts, level, msg FROM (SELECT id, ts, level, msg FROM job_logs WHERE job_id=? ORDER BY id DESC LIMIT ?) ORDER BY id`, jobID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLogs(rows)
}

func scanLogs(rows *sql.Rows) ([]LogLine, error) {
	var out []LogLine
	for rows.Next() {
		var l LogLine
		var ts int64
		if err := rows.Scan(&l.ID, &ts, &l.Level, &l.Msg); err != nil {
			return nil, err
		}
		l.Time = time.UnixMilli(ts)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Purge, verilen tarihten önce başlamış işleri ve loglarını siler.
func (s *Store) Purge(before time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	cut := before.Unix()
	sub := `SELECT id FROM jobs WHERE started_at < ? AND status <> ?`
	if _, err := tx.Exec(`DELETE FROM job_logs WHERE job_id IN (`+sub+`)`, cut, STATUS_RUNNING); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM job_vms WHERE job_id IN (`+sub+`)`, cut, STATUS_RUNNING); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM jobs WHERE started_at < ? AND status <> ?`, cut, STATUS_RUNNING)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// VMLast, bir VM'in en son yedekleme sonucudur.
type VMLast struct {
	JobID   int64
	Status  string
	Message string
	At      time.Time
}

// LastVMResults, her VM için en son yedekleme işindeki sonucu döndürür.
func (s *Store) LastVMResults() (map[int]VMLast, error) {
	rows, err := s.db.Query(`SELECT v.vmid, v.job_id, v.status, v.message, j.started_at
		FROM job_vms v JOIN jobs j ON j.id = v.job_id
		WHERE v.rowid IN (SELECT MAX(rowid) FROM job_vms GROUP BY vmid)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]VMLast{}
	for rows.Next() {
		var vmid int
		var l VMLast
		var at int64
		if err := rows.Scan(&vmid, &l.JobID, &l.Status, &l.Message, &at); err != nil {
			return nil, err
		}
		l.At = time.Unix(at, 0)
		out[vmid] = l
	}
	return out, rows.Err()
}

// Window, bir işin başlangıç ve bitiş zamanıdır.
type Window struct {
	Start, End time.Time
}

// BackupWindows, bitmiş tüm yedekleme işlerinin zaman aralıklarını döndürür.
func (s *Store) BackupWindows() ([]Window, error) {
	rows, err := s.db.Query(`SELECT started_at, finished_at FROM jobs WHERE kind=? AND finished_at > 0`, KIND_BACKUP)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out = append(out, Window{Start: time.Unix(a, 0), End: time.Unix(b, 0)})
	}
	return out, rows.Err()
}
