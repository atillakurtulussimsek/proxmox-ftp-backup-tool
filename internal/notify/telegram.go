// Package notify, Telegram üzerinden bildirim gönderir.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/atillakurtulussimsek/proxmox-ftp-backup-tool/internal/config"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

// Escape, Telegram HTML biçimi için metni kaçışlar.
func Escape(s string) string { return html.EscapeString(s) }

// SendTelegram, HTML biçimli bir mesaj gönderir. Hata mesajlarında token yer almaz.
func SendTelegram(ctx context.Context, cfg config.TelegramConfig, text string) error {
	if cfg.Token == "" || cfg.ChatID == "" {
		return errors.New("Telegram bot token veya chat ID ayarlanmamış")
	}
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	form := url.Values{
		"chat_id":                  {cfg.ChatID},
		"text":                     {text},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(cfg.Token) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("Telegram isteği oluşturulamadı")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		// url.Error token içeren adresi barındırır; yalnızca alttaki hatayı döndür.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("Telegram'a ulaşılamadı: %v", ue.Err)
		}
		return errors.New("Telegram'a ulaşılamadı")
	}
	defer resp.Body.Close()
	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if !body.OK {
		if body.Description == "" {
			body.Description = resp.Status
		}
		return fmt.Errorf("Telegram hatası: %s", body.Description)
	}
	return nil
}
