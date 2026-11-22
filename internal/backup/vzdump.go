package backup

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runVzdump, tek bir VM için vzdump çalıştırır ve çıktısını satır satır logFn'e iletir.
// İptal edildiğinde vzdump'a SIGTERM gönderilir ki snapshot/kilit temizliği yapabilsin.
func runVzdump(ctx context.Context, vmid int, dumpDir, mode, compress string, logFn func(string)) error {
	args := []string{
		strconv.Itoa(vmid),
		"--dumpdir", dumpDir,
		"--mode", mode,
		"--compress", compress,
		"--remove", "0",
	}
	logFn("Komut: vzdump " + strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "vzdump", args...)
	// Kendi süreç grubunda çalışır; iptalde önce yalnızca ana sürece SIGTERM gider,
	// vzdump kendi temizliğini yapar. 3 dakikada bitmezse SIGKILL uygulanır.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Minute

	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("vzdump çalıştırılamadı: %w", err)
	}
	pw.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			if line := strings.TrimRight(sc.Text(), "\r"); line != "" {
				logFn("vzdump: " + line)
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err = cmd.Wait()
	if ctx.Err() != nil {
		// İptal sonrası artakalan alt süreçler çıktı borusunu açık tutmasın.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	wg.Wait()
	pr.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("vzdump başarısız: %w", err)
	}
	return nil
}

// findDumpFiles, vzdump'ın bu çalışmada ürettiği dosyaları bulur: verilen VM'e ait,
// since sonrasında değişmiş en yeni zaman damgalı set. Geçici dosyalar hariç tutulur.
func findDumpFiles(dumpDir string, vmid int, since time.Time) ([]string, error) {
	entries, err := os.ReadDir(dumpDir)
	if err != nil {
		return nil, err
	}
	byStamp := map[string][]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		bf, ok := ParseName(e.Name())
		if !ok || bf.VMID != vmid || strings.HasSuffix(bf.Ext, ".tmp") || strings.HasSuffix(bf.Ext, ".dat") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(since.Add(-time.Minute)) {
			continue
		}
		byStamp[bf.Stamp] = append(byStamp[bf.Stamp], filepath.Join(dumpDir, e.Name()))
	}
	if len(byStamp) == 0 {
		return nil, fmt.Errorf("vzdump çıktı dosyası bulunamadı (%s)", dumpDir)
	}
	stamps := make([]string, 0, len(byStamp))
	for s := range byStamp {
		stamps = append(stamps, s)
	}
	sort.Strings(stamps)
	files := byStamp[stamps[len(stamps)-1]]
	hasArchive := false
	for _, f := range files {
		if bf, _ := ParseName(filepath.Base(f)); bf.IsArchive() {
			hasArchive = true
		}
	}
	if !hasArchive {
		return nil, fmt.Errorf("yedek arşivi bulunamadı, yalnızca: %s", strings.Join(baseNames(files), ", "))
	}
	// Arşiv önce, log en son yüklensin.
	sort.Slice(files, func(i, j int) bool {
		ai, _ := ParseName(filepath.Base(files[i]))
		aj, _ := ParseName(filepath.Base(files[j]))
		if ai.IsArchive() != aj.IsArchive() {
			return ai.IsArchive()
		}
		return files[i] < files[j]
	})
	return files, nil
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}
