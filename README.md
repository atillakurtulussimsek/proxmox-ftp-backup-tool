# pve-ftp-backup

Proxmox VE host'unda çalışan, VM ve LXC yedeklerini `vzdump` ile alıp FTP / FTPS / SFTP
sunucusuna yükleyen, yüklemeyi doğruladıktan sonra yerel kopyayı silen ve uzak sunucudaki
eski yedekleri temizleyen tek binary'lik web yönetim aracı.

- Go + HTMX, harici CDN yok; şablonlar, CSS ve HTMX binary'ye gömülü.
- Ayarlar: `/etc/pve-ftp-backup/config.json` (0600, root)
- İş geçmişi ve loglar: `/var/lib/pve-ftp-backup/data.db` (SQLite, CGO gerektirmez)
- Zamanlayıcı uygulamanın içinde çalışır (sistem cron'u kullanılmaz).

## Nasıl çalışır?

Yerel disk az olduğu için VM'ler **tek tek ve sırayla** işlenir:

1. Geçici klasörde (varsayılan `/var/lib/vz/dump`) VM'in disk boyutu (`maxdisk`) kadar boş
   alan var mı kontrol edilir. Yoksa yalnızca o VM atlanır.
2. `vzdump <vmid> --dumpdir <klasör> --mode .. --compress .. --remove 0` çalışır.
3. Oluşan arşiv ve log dosyası kendi klasörüne `uploading-<ad>` geçici adıyla yüklenir, uzak
   boyut yerel boyutla karşılaştırılır, ardından asıl adına taşınır.
4. Doğrulama başarılıysa yerel dosyalar silinir. Başarısızsa yerel dosyalar **korunur** ve VM
   hatalı işaretlenir.
5. Sonraki VM'e geçilir.

Uzak sunucudaki klasör düzeni (`<uzak klasör>` ayarlarda girilen klasördür):

```
<uzak klasör>/backup/<vmid>/vzdump-qemu-101-2026_11_23-02_00_00.vma.zst
<uzak klasör>/logs/<vmid>/vzdump-qemu-101-2026_11_23-02_00_00.log
```

`.log` dosyaları `logs/` altına, arşiv ve diğer vzdump dosyaları (ör. `.notes`) `backup/`
altına gider; klasörler gerektiğinde otomatik oluşturulur.

Tüm VM'ler bitince **saklama temizliği** çalışır: `backup/<vmid>` ve `logs/<vmid>` altındaki
`vzdump-(qemu|lxc)-<vmid>-<YYYY_MM_DD-HH_MM_SS>.*` dosyaları VM ve zaman damgasına göre
gruplanır; her VM için en yeni KEEP adet zaman damgası tutulur, daha eskilerin arşiv ve log
dosyaları birlikte silinir. Bu düzene uymayan dosyalara (kökteki dosyalar, yanlış klasördeki
ya da biçime uymayan dosyalar) dokunulmaz. Bu çalışmada yedeği başarısız olan ya da
atlanan VM'lerin eski yedekleri o işte temizlenmez.

Aynı anda yalnızca bir iş çalışır; iş sürerken tetiklenen yeni iş reddedilir ve geçmişe
"Reddedildi" olarak yazılır. Cluster'da yalnızca uygulamanın çalıştığı node'un VM'leri
listelenir ve yedeklenir.

## Kurulum

Derleme (Go 1.26+ gerekir; herhangi bir makinede yapılabilir):

```bash
make build
```

Çıktı `build/pve-ftp-backup` (linux/amd64, statik). Proxmox'a kopyalamak için paket:

```bash
make dist
```

Hazır paket her sürümde GitHub Releases sayfasında da bulunur. Paketi Proxmox host'una kopyalayın, açın ve root olarak kurun:

```bash
tar xzf pve-ftp-backup-*-linux-amd64.tar.gz && cd pve-ftp-backup && ./install.sh
```

Kaynak kod Proxmox üzerindeyse doğrudan `make install` de kullanılabilir.

`install.sh` şunları yapar: binary'yi `/usr/local/bin`'e kopyalar, `/etc/pve-ftp-backup` ve
`/var/lib/pve-ftp-backup` klasörlerini oluşturur, systemd servisini kurup etkinleştirir ve
şifre belirlenmemişse arayüz kullanıcı adı/şifresini sorar. Güncelleme için aynı betiği
tekrar çalıştırmak yeterlidir; mevcut ayarlar korunur.

Arayüz: `http://<proxmox-ip>:8090`

## İlk ayarlar

1. **Ayarlar → FTP sunucusu:** protokol, sunucu, port, kullanıcı, şifre ve uzak klasörü girin.
   "Bağlantıyı Test Et" uzak klasörü gerekirse oluşturur, küçük bir test dosyası yazıp siler.
   Kaydet'e basın.
   - Şifre ve Telegram token'ı arayüzde hiçbir zaman gösterilmez; alan boş bırakılırsa mevcut
     değer korunur.
   - FTP/FTPS yalnızca pasif modda çalışır. NAT arkasındaki sunucularda sorun olursa
     "EPSV'yi kapat" seçeneğini deneyin.
   - SFTP'de sunucu anahtarı ilk bağlantıda kaydedilir (TOFU); sonraki bağlantılarda anahtar
     değişirse bağlantı reddedilir. Sunucuyu bilerek değiştirdiyseniz "SFTP sunucu anahtarını
     sıfırla" seçeneğini kullanın.
2. **Yedekleme ve saklama:** yedeklenecek VM/LXC'leri işaretleyin; mod, sıkıştırma, geçici
   klasör ve varsayılan KEEP değerini (7) belirleyin. VM bazında farklı KEEP girilebilir.
3. **Zamanlama:** cron ifadesi girin (ör. `0 2 * * *` her gün 02:00) ve açın. Yazarken
   okunabilir açıklama ve sonraki çalışma zamanları gösterilir.
4. **Hız sınırı (isteğe bağlı):** FTP'ye yükleme ve FTP'den indirme (Proxmox'a geri alma,
   tarayıcıya indirme) için ayrı ayrı Mbit/sn sınırı girin; boş bırakılırsa sınırsızdır.
   Değişiklik süren aktarımlara da birkaç saniye içinde uygulanır.
5. **Telegram (isteğe bağlı):** bot token ve chat ID girin, hangi bildirimlerin gönderileceğini
   seçin, "Test mesajı gönder" ile deneyin.
6. **Kontrol Paneli → Şimdi Yedekle** ile ilk yedeği elle alıp sonucu izleyin.

### Ayar dosyasından değiştirilenler

`/etc/pve-ftp-backup/config.json` içindeki şu alanlar arayüzden değiştirilmez; düzenleyip
`systemctl restart pve-ftp-backup` çalıştırın:

| Alan | Açıklama | Varsayılan |
|---|---|---|
| `listen` | Dinleme adresi | `0.0.0.0:8090` |
| `tlsCert`, `tlsKey` | İkisi de doluysa HTTPS ile çalışır | boş |

Örnek: Proxmox'un kendi sertifikasını kullanmak için
`"tlsCert": "/etc/pve/local/pve-ssl.pem", "tlsKey": "/etc/pve/local/pve-ssl.key"`.

Arayüz şifresini değiştirmek için:

```bash
pve-ftp-backup set-password
```

## Kullanım

- **Kontrol Paneli:** çalışan işin canlı durumu (hangi VM, aşama, yükleme yüzdesi ve hızı,
  son log satırları, İptal butonu); son iş, sonraki çalışma, FTP kullanımı ve geçici alan
  özeti; tüm VM'ler tek tabloda: son yedeğin yaşı (36 saate kadar yeşil, 7 güne kadar sarı,
  daha eskiyse kırmızı), son denemenin sonucu, FTP'deki set sayısı / KEEP, tek VM yedekleme.
  Tablo ad/ID ile aranabilir, "Yedeklenenler" ve "Sorunlu" filtreleri vardır.
- **Yedekler:** FTP'deki yedekler VM bazında gruplanır; toplam ve VM bazında kullanılan alan
  gösterilir. Satır işlemleri: İndir (tarayıcıya akış), Proxmox'a geri al, Sil.
- **Geçmiş:** tüm işler; satıra tıklayınca VM sonuçları ve tam log açılır. Kayıtlar
  varsayılan olarak 90 gün saklanır (Ayarlar → Genel).

## Güncelleme

**Ayarlar → Güncelleme → Güncellemeleri kontrol et** GitHub'daki son kararlı sürümü
gösterir. Yeniyse sürüm notlarıyla birlikte **Güncelle** butonu çıkar. Basınca:

1. Yeni iş başlatılması engellenir (çalışan bir iş varsa güncelleme reddedilir).
2. `pve-ftp-backup-linux-amd64` indirilir, release'teki `SHA256SUMS` ile doğrulanır ve
   `version` komutuyla çalıştırılıp sürümü kontrol edilir.
3. Binary atomik olarak değiştirilir ve servis `systemctl restart` ile yeniden başlatılır;
   sayfa yeni sürüm açılınca kendini yeniler.

Doğrulama başarısız olursa eski binary olduğu gibi kalır. Kontrol yalnızca elle yapılır.
Güncelleme yalnızca binary'yi değiştirir; systemd unit dosyasındaki değişiklikler için
`install.sh` tekrar çalıştırılmalıdır.

### Sürüm yayımlama

```bash
git tag v0.2.0
```

```bash
git push origin v0.2.0
```

`.github/workflows/release.yml` testleri çalıştırır, derler ve şu dosyalarla GitHub Release
oluşturur: `pve-ftp-backup-linux-amd64`, `pve-ftp-backup-<sürüm>-linux-amd64.tar.gz`,
`SHA256SUMS`. Etiket `vMAJOR.MINOR.PATCH` biçiminde olmalıdır; `-` içeren etiketler
(ör. `v0.2.0-rc1`) ön sürüm olarak yayımlanır ve güncelleyici tarafından görülmez.

## Geri yükleme

1. **Yedekler** sayfasında ilgili arşivin (`.vma.zst`, `.tar.zst` vb.) yanındaki
   **Proxmox'a geri al** butonuna basın. Dosya geçici yedek klasörüne
   (varsayılan `/var/lib/vz/dump`) indirilir; ilerlemeyi Kontrol Paneli'nden izleyebilirsiniz.
2. Proxmox arayüzünde **Datacenter → <node> → local → Backups** altında dosya görünür.
   Seçip **Restore** ile geri yükleyin (aynı ya da yeni VMID).
3. Komut satırından da yapılabilir:

```bash
qmrestore /var/lib/vz/dump/vzdump-qemu-100-2024_05_01-02_00_00.vma.zst 100
```

```bash
pct restore 101 /var/lib/vz/dump/vzdump-lxc-101-2024_05_01-02_00_00.tar.zst --storage local-lvm
```

4. Geri yükleme bitince yerel dosyayı silerek alanı boşaltın.

Geçici klasör `local` depolamasının dump klasörü değilse dosya Proxmox arayüzünde görünmez;
bu durumda komut satırıyla geri yükleyin.

## Sorun giderme

| Belirti | Çözüm |
|---|---|
| Servis açılmıyor: "arayüz şifresi belirlenmemiş" | `pve-ftp-backup set-password` çalıştırıp servisi yeniden başlatın. |
| Servis logları | `journalctl -u pve-ftp-backup -f` |
| VM listesi boş / "pvesh hatası" | Servis root olarak mı çalışıyor? `pvesh get /cluster/resources --type vm --output-format json` elle çalışıyor mu? Node adı `hostname -s` ile aynı olmalı. |
| VM "Atlandı: yetersiz boş alan" | Geçici klasörde VM'in disk boyutu kadar boş alan gerekir. Başka bir disk/klasör seçin veya yer açın. |
| FTP "connection refused" / zaman aşımı | Port ve güvenlik duvarını kontrol edin. Pasif mod port aralığının sunucuda açık olduğundan emin olun. |
| Liste/yükleme takılıyor (NAT) | "EPSV'yi kapat" seçeneğini açın. |
| FTPS sertifika hatası | Kendinden imzalı sertifikada "FTPS sertifika doğrulamasını atla" seçeneğini açın. |
| SFTP "sunucu anahtarı değişmiş" | Sunucu değiştiyse anahtarı sıfırlayın; değişmediyse araya giren bir cihaz olabilir, inceleyin. |
| "boyut uyuşmuyor" | Uzak diskte yer kalmamış veya bağlantı kopmuş olabilir. Yerel dosyalar korunur; sorunu giderip VM'i tekrar yedekleyin, sonra geçici klasördeki eski dosyayı elle silin. |
| Yerelde kalmış yedek dosyaları | Hatalı işlerde dosyalar silinmez. `ls -lh /var/lib/vz/dump` ile kontrol edip gereksizleri silin. |
| "Başka bir iş çalışıyor" | Kontrol Paneli'nden çalışan işi bekleyin veya iptal edin. |
| İptal edilen yedekte VM kilitli kaldı | `qm unlock <vmid>` veya `pct unlock <vmid>`. |
| Telegram mesajı gelmiyor | Test mesajını deneyin; bota en az bir mesaj gönderdiğinizden ve chat ID'nin doğru olduğundan emin olun. |

## Geliştirme

```bash
make test
```

Paket yapısı:

```
cmd/pve-ftp-backup     giriş noktası (serve, set-password, version)
internal/config        ayarlar (JSON, bcrypt)
internal/store         SQLite iş geçmişi ve loglar
internal/ftpclient     FTP / FTPS / SFTP ortak arayüzü, yol güvenliği
internal/backup        vzdump, yükleme, doğrulama, saklama, geri alma, tek iş kilidi
internal/scheduler     uygulama içi cron
internal/notify        Telegram
internal/updater       GitHub Releases'tan güncelleme
internal/web           HTTP sunucusu, Basic Auth, CSRF, HTMX şablonları
deploy/                systemd unit
.github/workflows      etiket push'lanınca release oluşturma
```
