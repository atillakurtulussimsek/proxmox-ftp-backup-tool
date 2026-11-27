// Package backup, vzdump ile yedek alma, FTP'ye yükleme, saklama temizliği ve
// geri alma işlerini yürütür. Aynı anda yalnızca bir iş çalışabilir.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/ftpclient"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/notify"
	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/store"
)

// ErrBusy, başka bir iş çalışırken döndürülür.
var ErrBusy = errors.New("başka bir iş çalışıyor; bitmesini bekleyin veya iptal edin")

// ErrUpdating, uygulama güncellenirken yeni iş başlatılmak istendiğinde döndürülür.
var ErrUpdating = errors.New("uygulama güncelleniyor; yeni iş başlatılamaz")

// UPLOAD_PREFIX, yükleme sürerken kullanılan geçici uzak ad önekidir. Bu önekli dosyalar
// vzdump biçimine uymadığı için saklama temizliğine hiçbir zaman dahil olmaz.
const UPLOAD_PREFIX = "uploading-"

// Transfer, sürmekte olan bir dosya aktarımıdır.
type Transfer struct {
	Name    string
	Done    int64
	Total   int64
	Started time.Time
	Limit   int64 // bayt/sn, 0 sınırsız (aktarım başladığındaki değer)
}

func (t Transfer) Percent() float64 {
	if t.Total <= 0 {
		return 0
	}
	return float64(t.Done) * 100 / float64(t.Total)
}

// Speed, ortalama aktarım hızıdır (bayt/sn).
func (t Transfer) Speed() float64 {
	el := time.Since(t.Started).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(t.Done) / el
}

// Status, çalışan işin anlık durumudur.
type Status struct {
	JobID      int64
	Kind       string
	Trigger    string
	Started    time.Time
	VMIndex    int
	VMCount    int
	VMID       int
	VMName     string
	Phase      string
	Transfer   *Transfer
	Cancelling bool
}

type Runner struct {
	cfg     *config.Manager
	st      *store.Store
	log     *slog.Logger
	baseCtx context.Context

	mu       sync.Mutex
	cur      *Status
	updating bool
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewRunner(baseCtx context.Context, cfg *config.Manager, st *store.Store, log *slog.Logger) *Runner {
	return &Runner{cfg: cfg, st: st, log: log, baseCtx: baseCtx}
}

// Status, çalışan işin kopyasını döndürür; iş yoksa false.
func (r *Runner) Status() (Status, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil {
		return Status{}, false
	}
	s := *r.cur
	if r.cur.Transfer != nil {
		t := *r.cur.Transfer
		s.Transfer = &t
	}
	return s, true
}

// Cancel, çalışan işi iptal eder.
func (r *Runner) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur == nil || r.cancel == nil {
		return false
	}
	r.cur.Cancelling = true
	r.cancel()
	return true
}

// Shutdown, çalışan işi iptal edip bitmesini en fazla timeout kadar bekler.
func (r *Runner) Shutdown(timeout time.Duration) {
	r.mu.Lock()
	done := r.done
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

type job struct {
	r  *Runner
	id int64
}

func (j *job) logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := j.r.st.AppendLog(j.id, level, msg); err != nil {
		j.r.log.Error("log yazılamadı", "err", err)
	}
	switch level {
	case "error":
		j.r.log.Error(msg, "job", j.id)
	case "warn":
		j.r.log.Warn(msg, "job", j.id)
	default:
		j.r.log.Info(msg, "job", j.id)
	}
}

func (j *job) info(format string, args ...any)  { j.logf("info", format, args...) }
func (j *job) warn(format string, args ...any)  { j.logf("warn", format, args...) }
func (j *job) error(format string, args ...any) { j.logf("error", format, args...) }

func (r *Runner) update(fn func(s *Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		fn(r.cur)
	}
}

// uploadRate ve downloadRate, ayarlardaki güncel hız sınırını bayt/sn olarak verir.
func (r *Runner) uploadRate() int64 { return config.BytesPerSec(r.cfg.Get().Bandwidth.UploadMbit) }
func (r *Runner) downloadRate() int64 {
	return config.BytesPerSec(r.cfg.Get().Bandwidth.DownloadMbit)
}

func (r *Runner) setPhase(phase string) {
	r.update(func(s *Status) { s.Phase = phase; s.Transfer = nil })
}

// begin, kilidi alır ve yeni bir iş kaydı oluşturur.
func (r *Runner) begin(kind, trigger string) (*job, context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updating {
		return nil, nil, ErrUpdating
	}
	if r.cur != nil {
		return nil, nil, ErrBusy
	}
	id, err := r.st.CreateJob(kind, trigger)
	if err != nil {
		return nil, nil, fmt.Errorf("iş kaydı oluşturulamadı: %w", err)
	}
	ctx, cancel := context.WithCancel(r.baseCtx)
	r.cur = &Status{JobID: id, Kind: kind, Trigger: trigger, Started: time.Now(), Phase: "Başlıyor"}
	r.cancel = cancel
	r.done = make(chan struct{})
	return &job{r: r, id: id}, ctx, nil
}

func (r *Runner) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	close(r.done)
	r.cur = nil
	r.cancel = nil
	r.done = nil
}

// HoldForUpdate, iş çalışmıyorsa yeni işlerin başlamasını engeller. Dönen fonksiyon
// engeli kaldırır (güncelleme başarısız olursa çağrılır).
func (r *Runner) HoldForUpdate() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		return nil, ErrBusy
	}
	if r.updating {
		return nil, ErrUpdating
	}
	r.updating = true
	return func() {
		r.mu.Lock()
		r.updating = false
		r.mu.Unlock()
	}, nil
}

// rejectIfBusy, kilit alınamadığında reddi geçmişe yazar.
func (r *Runner) rejectIfBusy(err error, kind, trigger string) {
	switch {
	case errors.Is(err, ErrBusy):
		_ = r.st.RecordRejected(kind, trigger, "Başka bir iş çalıştığı için reddedildi.")
	case errors.Is(err, ErrUpdating):
		_ = r.st.RecordRejected(kind, trigger, "Uygulama güncellendiği için reddedildi.")
	}
}

// Connect, ayarlardaki sunucuya bağlanır ve uzak kök klasörün varlığını sağlar.
// SFTP'de sunucu anahtarı ilk kez görülüyorsa ayarlara kaydedilir.
func Connect(ctx context.Context, mgr *config.Manager) (ftpclient.Client, error) {
	cfg := mgr.Get()
	c, err := ftpclient.New(ctx, cfg.FTP)
	if err != nil {
		return nil, err
	}
	if hk, ok := c.(ftpclient.HostKeyReporter); ok && cfg.FTP.HostKey == "" && hk.HostKey() != "" {
		key := hk.HostKey()
		_ = mgr.Update(func(n *config.Config) error {
			if n.FTP.HostKey == "" && n.FTP.Host == cfg.FTP.Host && n.FTP.Port == cfg.FTP.Port {
				n.FTP.HostKey = key
			}
			return nil
		})
	}
	if err := c.EnsureRoot(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// ---------------------------------------------------------------- yedekleme

// StartBackup, verilen VM'ler için yedekleme işini arka planda başlatır.
func (r *Runner) StartBackup(trigger string, vmids []int) (int64, error) {
	cfg := r.cfg.Get()
	if len(vmids) == 0 {
		return 0, errors.New("yedeklenecek VM seçilmemiş")
	}
	if !cfg.FTPConfigured() {
		return 0, errors.New("FTP ayarları eksik")
	}
	j, ctx, err := r.begin(store.KIND_BACKUP, trigger)
	if err != nil {
		r.rejectIfBusy(err, store.KIND_BACKUP, trigger)
		return 0, err
	}
	r.update(func(s *Status) { s.VMCount = len(vmids) })
	go r.runBackup(ctx, j, cfg, append([]int(nil), vmids...))
	return j.id, nil
}

// CleanupResult, saklama temizliğinin sonucudur.
type CleanupResult struct {
	Deleted   int
	Freed     int64
	Failed    int
	TotalUsed int64
}

func (r *Runner) runBackup(ctx context.Context, j *job, cfg config.Config, vmids []int) {
	defer r.end()
	start := time.Now()
	j.info("Yedekleme başladı. VM sayısı: %d, mod: %s, sıkıştırma: %s, geçici klasör: %s",
		len(vmids), cfg.Backup.Mode, cfg.Backup.Compress, cfg.Backup.DumpDir)

	r.cleanDumpDir(j, cfg.Backup.DumpDir)

	r.setPhase("VM listesi alınıyor")
	guests, err := ListGuests(ctx)
	if err != nil {
		j.error("VM listesi alınamadı: %v", err)
		_ = r.st.FinishJob(j.id, store.STATUS_FAILED, "VM listesi alınamadı: "+err.Error(), 0)
		r.notifyBackup(cfg, j, nil, nil, store.STATUS_FAILED, time.Since(start), 0, err.Error())
		return
	}
	gmap := make(map[int]Guest, len(guests))
	for _, g := range guests {
		gmap[g.VMID] = g
	}

	var results []store.VMResult
	notOK := map[int]bool{}
	var uploaded int64
	for i, id := range vmids {
		var res store.VMResult
		if ctx.Err() != nil {
			res = store.VMResult{VMID: id, Name: gmap[id].Name, Status: store.STATUS_CANCELLED, Message: "İş iptal edildi"}
		} else {
			r.update(func(s *Status) {
				s.VMIndex, s.VMID, s.VMName, s.Transfer = i+1, id, gmap[id].Name, nil
			})
			res = r.backupOne(ctx, j, cfg, id, gmap)
		}
		results = append(results, res)
		if err := r.st.AddVMResult(j.id, res); err != nil {
			j.error("VM sonucu kaydedilemedi: %v", err)
		}
		if res.Status == store.STATUS_SUCCESS {
			uploaded += res.Size
		} else {
			notOK[id] = true
		}
	}

	cancelled := ctx.Err() != nil
	var cleanup *CleanupResult
	var cleanupErr error
	if !cancelled {
		r.update(func(s *Status) { s.VMID, s.VMName = 0, "" })
		r.setPhase("Saklama temizliği")
		var skipped []string
		for id := range notOK {
			skipped = append(skipped, fmt.Sprint(id))
		}
		if len(skipped) > 0 {
			j.warn("Başarısız VM'lerin eski yedekleri bu işte temizlenmeyecek: %s", strings.Join(skipped, ", "))
		}
		res, err := r.retention(ctx, j, cfg, notOK)
		if err != nil {
			cleanupErr = err
			j.error("Saklama temizliği başarısız: %v", err)
		} else {
			cleanup = &res
		}
	}

	status := overallStatus(results, cancelled)
	if status == store.STATUS_SUCCESS && (cleanupErr != nil || (cleanup != nil && cleanup.Failed > 0)) {
		status = store.STATUS_PARTIAL
	}
	summary := vmSummary(results)
	if cleanup != nil {
		summary += fmt.Sprintf(" Temizlik: %d dosya silindi (%s).", cleanup.Deleted, HumanBytes(cleanup.Freed))
	}
	j.info("İş bitti (%s). %s Süre: %s, yüklenen: %s", statusText(status), summary, HumanDuration(time.Since(start)), HumanBytes(uploaded))
	_ = r.st.FinishJob(j.id, status, summary, uploaded)
	errText := ""
	if cleanupErr != nil {
		errText = "Saklama temizliği başarısız: " + cleanupErr.Error()
	}
	r.notifyBackup(cfg, j, results, cleanup, status, time.Since(start), uploaded, errText)
}

func overallStatus(results []store.VMResult, cancelled bool) string {
	if cancelled {
		return store.STATUS_CANCELLED
	}
	ok := 0
	for _, r := range results {
		if r.Status == store.STATUS_SUCCESS {
			ok++
		}
	}
	switch {
	case ok == len(results):
		return store.STATUS_SUCCESS
	case ok == 0:
		return store.STATUS_FAILED
	default:
		return store.STATUS_PARTIAL
	}
}

func vmSummary(results []store.VMResult) string {
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	parts := []string{}
	for _, st := range []string{store.STATUS_SUCCESS, store.STATUS_FAILED, store.STATUS_SKIPPED, store.STATUS_CANCELLED} {
		if counts[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], strings.ToLower(statusText(st))))
		}
	}
	return strings.Join(parts, ", ") + "."
}

func statusText(s string) string {
	switch s {
	case store.STATUS_SUCCESS:
		return "Başarılı"
	case store.STATUS_FAILED:
		return "Hatalı"
	case store.STATUS_PARTIAL:
		return "Kısmen başarılı"
	case store.STATUS_CANCELLED:
		return "İptal edildi"
	case store.STATUS_SKIPPED:
		return "Atlandı"
	case store.STATUS_RUNNING:
		return "Çalışıyor"
	case store.STATUS_REJECTED:
		return "Reddedildi"
	}
	return s
}

// StatusText, durum kodunun Türkçe karşılığını döndürür.
func StatusText(s string) string { return statusText(s) }

func (r *Runner) backupOne(ctx context.Context, j *job, cfg config.Config, vmid int, gmap map[int]Guest) store.VMResult {
	t0 := time.Now()
	res := store.VMResult{VMID: vmid}
	fail := func(status, format string, args ...any) store.VMResult {
		if ctx.Err() != nil && status == store.STATUS_FAILED {
			status = store.STATUS_CANCELLED
			format, args = "İş iptal edildi", nil
		}
		res.Status = status
		res.Message = fmt.Sprintf(format, args...)
		res.Duration = time.Since(t0)
		if status == store.STATUS_SKIPPED {
			j.warn("VM %d: %s", vmid, res.Message)
		} else {
			j.error("VM %d: %s", vmid, res.Message)
		}
		return res
	}

	g, ok := gmap[vmid]
	if !ok {
		return fail(store.STATUS_FAILED, "VM/LXC bu node üzerinde bulunamadı")
	}
	res.Name = g.Name
	j.info("VM %d (%s, %s) işleniyor", vmid, g.Name, g.Type)

	r.setPhase("Boş alan kontrolü")
	if err := os.MkdirAll(cfg.Backup.DumpDir, 0o755); err != nil {
		return fail(store.STATUS_FAILED, "geçici klasör oluşturulamadı: %v", err)
	}
	free, err := FreeSpace(cfg.Backup.DumpDir)
	if err != nil {
		return fail(store.STATUS_FAILED, "boş alan okunamadı: %v", err)
	}
	if g.MaxDisk > 0 && free < g.MaxDisk {
		return fail(store.STATUS_SKIPPED, "yetersiz boş alan: gerekli %s, mevcut %s", HumanBytes(g.MaxDisk), HumanBytes(free))
	}
	j.info("Boş alan: %s (VM disk boyutu %s)", HumanBytes(free), HumanBytes(g.MaxDisk))

	r.setPhase("vzdump çalışıyor")
	dumpStart := time.Now()
	if err := runVzdump(ctx, vmid, cfg.Backup.DumpDir, cfg.Backup.Mode, cfg.Backup.Compress, func(line string) { j.info("%s", line) }); err != nil {
		return fail(store.STATUS_FAILED, "%v", err)
	}
	files, err := findDumpFiles(cfg.Backup.DumpDir, vmid, dumpStart)
	if err != nil {
		return fail(store.STATUS_FAILED, "%v", err)
	}
	j.info("Oluşan dosyalar: %s", strings.Join(baseNames(files), ", "))

	r.setPhase("FTP'ye bağlanılıyor")
	client, err := Connect(ctx, r.cfg)
	if err != nil {
		return fail(store.STATUS_FAILED, "FTP bağlantısı: %v (yerel dosyalar korundu)", err)
	}
	defer client.Close()

	var total int64
	for _, local := range files {
		name := filepath.Base(local)
		st, err := os.Stat(local)
		if err != nil {
			return fail(store.STATUS_FAILED, "yerel dosya okunamadı: %v", err)
		}
		size, err := r.uploadVerified(ctx, j, client, local, name, st.Size())
		if err != nil {
			return fail(store.STATUS_FAILED, "%s: %v (yerel dosyalar korundu)", name, err)
		}
		total += size
	}

	r.setPhase("Yerel dosyalar siliniyor")
	for _, local := range files {
		if err := os.Remove(local); err != nil {
			j.warn("Yerel dosya silinemedi: %s: %v", local, err)
		}
	}
	res.Status = store.STATUS_SUCCESS
	res.Size = total
	res.Duration = time.Since(t0)
	res.Message = fmt.Sprintf("%d dosya yüklendi", len(files))
	j.info("VM %d tamamlandı: %s yüklendi, süre %s", vmid, HumanBytes(total), HumanDuration(res.Duration))
	return res
}

// cleanDumpDir, geçici klasörde önceki işlerden kalanları siler. Hata işi durdurmaz.
func (r *Runner) cleanDumpDir(j *job, dumpDir string) {
	r.setPhase("Geçici klasör temizleniyor")
	windows, err := r.st.BackupWindows()
	if err != nil {
		j.warn("Geçici klasör temizlenemedi: iş geçmişi okunamadı: %v", err)
		return
	}
	left, err := FindLeftovers(dumpDir, windows)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			j.warn("Geçici klasör temizlenemedi: %v", err)
		}
		return
	}
	if len(left) == 0 {
		j.info("Geçici klasörde önceki işlerden kalan dosya yok")
		return
	}
	var freed int64
	removed := 0
	for _, l := range left {
		if err := RemoveLeftover(l); err != nil {
			j.warn("Kalıntı silinemedi: %s: %v", l.Path, err)
			continue
		}
		removed++
		freed += l.Size
		j.info("Önceki işten kalan silindi: %s (%s)", filepath.Base(l.Path), HumanBytes(l.Size))
	}
	j.info("Geçici klasör temizlendi: %d öğe, %s boşaldı", removed, HumanBytes(freed))
}

// uploadVerified, dosyayı hedef klasörüne (backup/<vmid> veya logs/<vmid>) geçici adla
// yükler, boyutunu doğrular ve asıl adına taşır.
func (r *Runner) uploadVerified(ctx context.Context, j *job, c ftpclient.Client, local, name string, size int64) (int64, error) {
	dst, err := RemotePath(name)
	if err != nil {
		return 0, err
	}
	dir := path.Dir(dst)
	if err := c.EnsureDir(ctx, dir); err != nil {
		return 0, err
	}
	tmp := path.Join(dir, UPLOAD_PREFIX+name)
	r.update(func(s *Status) {
		s.Phase = "Yükleniyor: " + name
		s.Transfer = &Transfer{Name: name, Total: size, Started: time.Now(), Limit: r.uploadRate()}
	})
	j.info("Yükleniyor: %s (%s)", name, HumanBytes(size))
	t0 := time.Now()
	progress := func(n int64) {
		r.update(func(s *Status) {
			if s.Transfer != nil {
				s.Transfer.Done = n
			}
		})
	}
	if err := c.Upload(ftpclient.WithRateLimit(ctx, r.uploadRate), local, tmp, progress); err != nil {
		_ = c.Delete(context.Background(), tmp)
		return 0, err
	}
	r.setPhase("Doğrulanıyor: " + name)
	remote, err := c.Size(ctx, tmp)
	if err != nil {
		_ = c.Delete(context.Background(), tmp)
		return 0, fmt.Errorf("doğrulama: %w", err)
	}
	if remote != size {
		_ = c.Delete(context.Background(), tmp)
		return 0, fmt.Errorf("boyut uyuşmuyor: yerel %d, uzak %d bayt", size, remote)
	}
	if err := c.Rename(ctx, tmp, dst); err != nil {
		_ = c.Delete(context.Background(), tmp)
		return 0, err
	}
	if remote, err = c.Size(ctx, dst); err != nil || remote != size {
		return 0, fmt.Errorf("taşıma sonrası doğrulama başarısız (uzak %d bayt): %v", remote, err)
	}
	el := time.Since(t0)
	speed := float64(size) / max(el.Seconds(), 0.001)
	j.info("Yüklendi ve doğrulandı: %s, %s, ortalama %s/sn", dst, HumanDuration(el), HumanBytes(int64(speed)))
	return size, nil
}

// ---------------------------------------------------------------- temizlik

// StartCleanup, yalnızca saklama temizliğini çalıştırır.
func (r *Runner) StartCleanup(trigger string) (int64, error) {
	cfg := r.cfg.Get()
	if !cfg.FTPConfigured() {
		return 0, errors.New("FTP ayarları eksik")
	}
	j, ctx, err := r.begin(store.KIND_CLEANUP, trigger)
	if err != nil {
		r.rejectIfBusy(err, store.KIND_CLEANUP, trigger)
		return 0, err
	}
	go func() {
		defer r.end()
		start := time.Now()
		r.setPhase("Saklama temizliği")
		res, err := r.retention(ctx, j, cfg, nil)
		status, summary := store.STATUS_SUCCESS, ""
		switch {
		case ctx.Err() != nil:
			status, summary = store.STATUS_CANCELLED, "İptal edildi."
		case err != nil:
			status, summary = store.STATUS_FAILED, err.Error()
			j.error("Temizlik başarısız: %v", err)
		default:
			summary = fmt.Sprintf("%d dosya silindi (%s).", res.Deleted, HumanBytes(res.Freed))
			if res.Failed > 0 {
				status = store.STATUS_PARTIAL
				summary += fmt.Sprintf(" %d dosya silinemedi.", res.Failed)
			}
		}
		_ = r.st.FinishJob(j.id, status, summary, 0)
		if cfg.Telegram.OnCleanup || (status != store.STATUS_SUCCESS && cfg.Telegram.OnFailure) {
			r.send(cfg, cleanupMessage(status, summary, res, err == nil, time.Since(start)))
		}
	}()
	return j.id, nil
}

func (r *Runner) retention(ctx context.Context, j *job, cfg config.Config, skip map[int]bool) (CleanupResult, error) {
	var res CleanupResult
	client, err := Connect(ctx, r.cfg)
	if err != nil {
		return res, err
	}
	defer client.Close()
	list, others, err := ListRemote(ctx, client)
	if err != nil {
		return res, err
	}
	for _, f := range list {
		res.TotalUsed += f.Size
	}
	for _, f := range others {
		res.TotalUsed += f.Size
	}
	groups, _ := Group(list)
	if len(others) > 0 {
		j.info("Klasör düzenine uymayan %d dosyaya dokunulmadı", len(others))
	}
	del := PlanRetention(groups, cfg.KeepFor, skip)
	if len(del) == 0 {
		j.info("Saklama: silinecek eski yedek yok")
		return res, nil
	}
	for _, f := range del {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		rp, err := RemotePath(f.Name)
		if err == nil {
			err = client.Delete(ctx, rp)
		}
		if err != nil {
			res.Failed++
			j.error("Silinemedi: %s: %v", f.Name, err)
			continue
		}
		res.Deleted++
		res.Freed += f.Size
		res.TotalUsed -= f.Size
		j.info("Silindi (KEEP=%d): %s (%s)", cfg.KeepFor(f.VMID), f.Name, HumanBytes(f.Size))
	}
	j.info("Saklama: %d dosya silindi, %s boşaldı. FTP'de kullanılan toplam alan: %s",
		res.Deleted, HumanBytes(res.Freed), HumanBytes(res.TotalUsed))
	return res, nil
}

// ---------------------------------------------------------------- geri alma

// StartRestore, uzak yedek arşivini yerel dump klasörüne indirir.
func (r *Runner) StartRestore(name string) (int64, error) {
	bf, ok := ParseName(name)
	if !ok || !bf.IsArchive() {
		return 0, errors.New("yalnızca vzdump yedek arşivleri geri alınabilir")
	}
	if err := ftpclient.ValidateName(name); err != nil {
		return 0, err
	}
	cfg := r.cfg.Get()
	dest := filepath.Join(cfg.Backup.DumpDir, name)
	if _, err := os.Stat(dest); err == nil {
		return 0, fmt.Errorf("dosya zaten yerelde mevcut: %s", dest)
	}
	j, ctx, err := r.begin(store.KIND_RESTORE, store.TRIGGER_MANUAL)
	if err != nil {
		return 0, err
	}
	go func() {
		defer r.end()
		r.update(func(s *Status) { s.VMID = bf.VMID; s.VMCount = 1; s.VMIndex = 1 })
		size, err := r.restore(ctx, j, cfg, name, dest)
		status, summary := store.STATUS_SUCCESS, fmt.Sprintf("%s indirildi (%s): %s", name, HumanBytes(size), dest)
		if err != nil {
			status, summary = store.STATUS_FAILED, err.Error()
			if ctx.Err() != nil {
				status = store.STATUS_CANCELLED
			}
			j.error("Geri alma başarısız: %v", err)
		} else {
			j.info("%s. Proxmox arayüzünde ilgili depolama > Backups altından 'Restore' ile geri yükleyebilirsiniz.", summary)
		}
		_ = r.st.FinishJob(j.id, status, summary, 0)
	}()
	return j.id, nil
}

func (r *Runner) restore(ctx context.Context, j *job, cfg config.Config, name, dest string) (int64, error) {
	rp, err := RemotePath(name)
	if err != nil {
		return 0, err
	}
	j.info("Geri alma: %s → %s", rp, dest)
	r.setPhase("FTP'ye bağlanılıyor")
	client, err := Connect(ctx, r.cfg)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	size, err := client.Size(ctx, rp)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(cfg.Backup.DumpDir, 0o755); err != nil {
		return 0, err
	}
	free, err := FreeSpace(cfg.Backup.DumpDir)
	if err != nil {
		return 0, fmt.Errorf("boş alan okunamadı: %w", err)
	}
	if free < size {
		return 0, fmt.Errorf("yetersiz boş alan: gerekli %s, mevcut %s", HumanBytes(size), HumanBytes(free))
	}
	tmp := filepath.Join(cfg.Backup.DumpDir, RESTORE_PREFIX+name)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	r.update(func(s *Status) {
		s.Phase = "İndiriliyor: " + name
		s.Transfer = &Transfer{Name: name, Total: size, Started: time.Now(), Limit: r.downloadRate()}
	})
	err = client.Download(ftpclient.WithRateLimit(ctx, r.downloadRate), rp, f, func(n int64) {
		r.update(func(s *Status) {
			if s.Transfer != nil {
				s.Transfer.Done = n
			}
		})
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if st, serr := os.Stat(tmp); serr != nil || st.Size() != size {
			err = fmt.Errorf("boyut uyuşmuyor: uzak %d bayt", size)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return size, nil
}

// ---------------------------------------------------------------- bildirim

func (r *Runner) send(cfg config.Config, text string) {
	if cfg.Telegram.Token == "" || cfg.Telegram.ChatID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := notify.SendTelegram(ctx, cfg.Telegram, text); err != nil {
		r.log.Warn("Telegram bildirimi gönderilemedi", "err", err)
	}
}

func (r *Runner) notifyBackup(cfg config.Config, j *job, results []store.VMResult, cleanup *CleanupResult, status string, dur time.Duration, uploaded int64, errText string) {
	if status == store.STATUS_SUCCESS && !cfg.Telegram.OnSuccess {
		return
	}
	if status != store.STATUS_SUCCESS && !cfg.Telegram.OnFailure {
		return
	}
	r.send(cfg, backupMessage(LocalNode(), j.id, results, cleanup, status, dur, uploaded, errText))
}

func statusIcon(s string) string {
	switch s {
	case store.STATUS_SUCCESS:
		return "✅"
	case store.STATUS_PARTIAL:
		return "⚠️"
	case store.STATUS_SKIPPED:
		return "⏭"
	case store.STATUS_CANCELLED:
		return "⛔"
	}
	return "❌"
}

func backupMessage(node string, jobID int64, results []store.VMResult, cleanup *CleanupResult, status string, dur time.Duration, uploaded int64, errText string) string {
	e := notify.Escape
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Yedekleme: %s</b>\n", statusIcon(status), e(statusText(status)))
	fmt.Fprintf(&b, "Node: %s · İş #%d\n", e(node), jobID)
	fmt.Fprintf(&b, "Süre: %s\nYüklenen: %s\n", HumanDuration(dur), HumanBytes(uploaded))
	if cleanup != nil {
		fmt.Fprintf(&b, "FTP kullanımı: %s\n", HumanBytes(cleanup.TotalUsed))
	}
	if len(results) > 0 {
		b.WriteString("\n")
		for _, r := range results {
			name := r.Name
			if name == "" {
				name = "-"
			}
			line := fmt.Sprintf("%s %d (%s)", statusIcon(r.Status), r.VMID, e(name))
			if r.Status == store.STATUS_SUCCESS {
				line += " – " + HumanBytes(r.Size)
			} else if r.Message != "" {
				msg := r.Message
				if len(msg) > 200 {
					msg = msg[:200] + "…"
				}
				line += " – " + e(msg)
			}
			b.WriteString(line + "\n")
		}
	}
	if cleanup != nil {
		fmt.Fprintf(&b, "\nTemizlik: %d dosya silindi (%s)", cleanup.Deleted, HumanBytes(cleanup.Freed))
		if cleanup.Failed > 0 {
			fmt.Fprintf(&b, ", %d silinemedi", cleanup.Failed)
		}
		b.WriteString("\n")
	}
	if errText != "" {
		fmt.Fprintf(&b, "\n%s\n", e(errText))
	}
	return b.String()
}

func cleanupMessage(status, summary string, res CleanupResult, hasUsage bool, dur time.Duration) string {
	e := notify.Escape
	var b strings.Builder
	fmt.Fprintf(&b, "%s <b>Saklama temizliği: %s</b>\n", statusIcon(status), e(statusText(status)))
	fmt.Fprintf(&b, "Node: %s\nSüre: %s\n%s\n", e(LocalNode()), HumanDuration(dur), e(summary))
	if hasUsage {
		fmt.Fprintf(&b, "FTP kullanımı: %s\n", HumanBytes(res.TotalUsed))
	}
	return b.String()
}
