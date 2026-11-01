// Package bot is the Telegram front end. Every action goes through
// kiosk.Service so the bot and the web UI behave the same.
package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/rnkch/flimmerkiste/internal/access"
	"github.com/rnkch/flimmerkiste/internal/kiosk"
)

const pageSize = 5

type Bot struct {
	k       *kiosk.Service
	b       *tg.Bot
	groupID int64

	mu      sync.Mutex
	pending map[int64]pendingInput // per user: what the next ForceReply answer is for
}

type pendingInput struct {
	action  string // rename | duration | position
	mediaID int64
}

func New(token string, groupID int64, k *kiosk.Service) (*Bot, error) {
	bt := &Bot{k: k, groupID: groupID, pending: map[int64]pendingInput{}}
	b, err := tg.New(token,
		tg.WithDefaultHandler(bt.handle),
		// One update at a time keeps uploads queued in the order they were sent.
		tg.WithNotAsyncHandlers(),
		tg.WithErrorsHandler(func(err error) { slog.Error("telegram", "err", err) }),
	)
	if err != nil {
		return nil, err
	}
	bt.b = b
	k.Fetch = bt.fetch
	return bt, nil
}

// Run polls Telegram and reports upload progress until ctx ends.
func (bt *Bot) Run(ctx context.Context) {
	bt.b.SetMyCommands(ctx, &tg.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "menu", Description: "Menü anzeigen"},
		{Command: "slides", Description: "Playlist verwalten"},
		{Command: "status", Description: "Status: Wiedergabe, Speicher, Temperatur"},
		{Command: "next", Description: "Nächstes Bild/Video"},
		{Command: "prev", Description: "Vorheriges Bild/Video"},
		{Command: "pause", Description: "Pause/Fortsetzen"},
		{Command: "lock", Description: "Sperren/Entsperren: nichts unterbricht die Wiedergabe"},
		{Command: "restart", Description: "Player neu starten"},
		{Command: "upgrade", Description: "Auf die neueste Version aktualisieren"},
		{Command: "display", Description: "mpv-Statistik auf dem Bildschirm"},
		{Command: "qr", Description: "QR-Code zur Weboberfläche zeigen"},
		{Command: "screenshot", Description: "Foto vom aktuellen Bildschirm"},
		{Command: "passwort", Description: "Neues Passwort für die Weboberfläche"},
		{Command: "debug", Description: "Debug-Modus"},
		{Command: "help", Description: "Hilfe"},
	}})
	bt.send(ctx, "📺 Flimmerkiste ist wieder online! (Version "+kiosk.Version()+")", nil)
	go bt.watchJobs(ctx)
	bt.b.Start(ctx)
}

func (bt *Bot) handle(ctx context.Context, _ *tg.Bot, u *models.Update) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("bot handler panic", "panic", r)
		}
	}()
	switch {
	case u.CallbackQuery != nil:
		bt.onCallback(ctx, u.CallbackQuery)
	case u.Message != nil:
		if u.Message.Chat.ID != bt.groupID {
			slog.Warn("rejected chat", "chat_id", u.Message.Chat.ID)
			return
		}
		bt.onMessage(ctx, u.Message)
	}
}

var streamURL = regexp.MustCompile(`(?i)^rtsps?://[\w./:@?&=%\-~!$'()*+,;]+`)

// downloadURL matches a message that is nothing but an http(s) link.
var downloadURL = regexp.MustCompile(`(?i)^https?://\S+$`)

func (bt *Bot) onMessage(ctx context.Context, m *models.Message) {
	if len(m.NewChatMembers) > 0 {
		bt.reply(ctx, m, bt.k.Setting(ctx, kiosk.SettingHelpText))
		return
	}
	if bt.onMedia(ctx, m) {
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		bt.onCommand(ctx, m, text)
		return
	}
	if m.ReplyToMessage != nil && bt.onReplyInput(ctx, m, text) {
		return
	}
	if u := streamURL.FindString(text); u != "" {
		if err := bt.k.Stream(ctx, u); errors.Is(err, kiosk.ErrLocked) {
			bt.reply(ctx, m, warn(err))
			return
		} else if err != nil {
			bt.reply(ctx, m, "⚠️ Stream konnte nicht geladen werden: "+err.Error())
			return
		}
		bt.reply(ctx, m, "▶️ Streaming: "+u+"\n\nZurück zur Playlist: /restart")
		return
	}
	if downloadURL.MatchString(text) {
		bt.onDownloadURL(ctx, m, text)
		return
	}
	if rest, ok := cutPrefixFold(text, "updatehilfe"); ok {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			bt.reply(ctx, m, "⚠️ Syntax: updatehilfe <Text>")
			return
		}
		bt.k.SetSetting(ctx, kiosk.SettingHelpText, rest)
		bt.reply(ctx, m, "📝 Hilfetext gespeichert!")
		return
	}
	switch text {
	case "Upload Media":
		bt.reply(ctx, m, "Sende ein Foto oder Video. Beschriftung (optional): <Reihenfolge> <Name> [<Sekunden>]\n\nGrößere Dateien als 20 MB über die Weboberfläche hochladen.\n\nNutze /slides zum Verwalten.")
	case "Slides":
		bt.showSlides(ctx, m.Chat.ID, 0, 1)
	case "Reload Playlist":
		bt.restart(ctx, m, "")
	case "Next":
		bt.control(ctx, m, bt.k.Next, "⏭️ Nächstes Bild/Video")
	case "Prev":
		bt.control(ctx, m, bt.k.Prev, "⏮️ Vorheriges Bild/Video")
	case "Pause", "Pause/Resume":
		bt.control(ctx, m, bt.k.TogglePause, "⏯️ Wiedergabe pausiert/fortgesetzt")
	case "Status":
		bt.reply(ctx, m, bt.statusText(ctx))
	case "Help":
		bt.reply(ctx, m, bt.k.Setting(ctx, kiosk.SettingHelpText))
	default:
		if l := strings.ToLower(text); l == "help" || l == "hilfe" {
			bt.reply(ctx, m, bt.k.Setting(ctx, kiosk.SettingHelpText))
		}
	}
}

func (bt *Bot) onCommand(ctx context.Context, m *models.Message, text string) {
	f := strings.Fields(text)
	cmd, _, _ := strings.Cut(strings.ToLower(strings.TrimPrefix(f[0], "/")), "@")
	arg := ""
	if len(f) > 1 {
		arg = strings.ToLower(f[1])
	}
	switch cmd {
	case "menu", "m":
		bt.showMenu(ctx, m)
	case "slides", "s":
		bt.showSlides(ctx, m.Chat.ID, 0, 1)
	case "restart", "r":
		bt.restart(ctx, m, arg)
	case "next", "n":
		bt.control(ctx, m, bt.k.Next, "⏭️ Nächstes Bild/Video")
	case "prev", "p":
		bt.control(ctx, m, bt.k.Prev, "⏮️ Vorheriges Bild/Video")
	case "pause", "pa":
		bt.control(ctx, m, bt.k.TogglePause, "⏯️ Wiedergabe pausiert/fortgesetzt")
	case "help", "h":
		bt.reply(ctx, m, bt.k.Setting(ctx, kiosk.SettingHelpText))
	case "display", "di":
		bt.control(ctx, m, bt.k.ToggleStats, "📺 mpv-Statistik (i) auf dem Bildschirm umgeschaltet")
	case "status", "st":
		bt.reply(ctx, m, bt.statusText(ctx))
	case "qr":
		bt.qr(ctx, m)
	case "screenshot", "sc":
		bt.screenshot(ctx, m)
	case "passwort", "password", "pw":
		bt.resetPassword(ctx, m)
	case "upgrade", "update":
		bt.upgrade(ctx, m, arg)
	case "lock", "sperre":
		bt.lock(ctx, m, arg)
	case "debug", "d":
		on := bt.k.Debug(ctx)
		switch arg {
		case "on":
			on = true
		case "off":
			on = false
		case "status":
		default:
			on = !on
		}
		bt.k.SetSetting(ctx, kiosk.SettingDebug, fmt.Sprint(on))
		bt.reply(ctx, m, map[bool]string{true: "🔍 Debug: ON", false: "🔍 Debug: OFF"}[on])
	}
}

// warn is the reply to an action that failed.
func warn(err error) string {
	if errors.Is(err, kiosk.ErrLocked) {
		return "🔒 Gesperrt: das würde die Wiedergabe unterbrechen. Entsperren mit /lock"
	}
	return "⚠️ " + err.Error()
}

// upgrade says whether a newer release exists; "/upgrade jetzt" installs it
// (two steps: it restarts the player and the whole screen goes dark).
func (bt *Bot) upgrade(ctx context.Context, m *models.Message, arg string) {
	info, err := bt.k.CheckUpgrade(ctx, true)
	switch {
	case errors.Is(err, kiosk.ErrUpgradeOff):
		bt.reply(ctx, m, "⚠️ Aktualisierungen sind abgeschaltet.")
		return
	case errors.Is(err, kiosk.ErrNoRelease):
		bt.reply(ctx, m, "ℹ️ Es wurde noch keine Version veröffentlicht. Läuft: "+kiosk.Version())
		return
	case err != nil:
		bt.reply(ctx, m, "⚠️ Prüfen fehlgeschlagen: "+err.Error())
		return
	case !info.Newer:
		bt.reply(ctx, m, "✅ Flimmerkiste ist aktuell ("+info.Current+").")
		return
	case !info.CanInstall:
		bt.reply(ctx, m, fmt.Sprintf("ℹ️ Version %s ist da, hat aber keinen Download für dieses Gerät.\n%s", info.Latest, info.URL))
		return
	}
	if arg != "jetzt" && arg != "now" {
		bt.reply(ctx, m, fmt.Sprintf("⬆️ Neue Version %s (läuft: %s).\n%s\n\nInstallieren: /upgrade jetzt\nDabei startet der Dienst neu: der Bildschirm wird kurz dunkel und die Wiedergabe beginnt von vorn.", info.Latest, info.Current, info.URL))
		return
	}
	bt.reply(ctx, m, "⬇️ Lade "+info.Latest+" herunter und prüfe sie…")
	res, err := bt.k.Upgrade(ctx)
	switch {
	case err != nil:
		bt.reply(ctx, m, "⚠️ Nicht aktualisiert: "+upgradeErrText(err))
	case res.Restarted:
		bt.reply(ctx, m, "✅ "+res.To+" ist installiert, Flimmerkiste startet neu und meldet sich gleich wieder.")
	default:
		bt.reply(ctx, m, "✅ "+res.To+" ist installiert. Flimmerkiste muss von Hand neu gestartet werden.")
	}
}

// upgradeErrText explains the errors an upgrade can end with.
func upgradeErrText(err error) string {
	switch {
	case errors.Is(err, kiosk.ErrLocked):
		return "gesperrt: das würde die Wiedergabe unterbrechen. Entsperren mit /lock"
	case errors.Is(err, kiosk.ErrChecksum):
		return "der Download passt nicht zur Prüfsumme; nichts wurde geändert."
	case errors.Is(err, kiosk.ErrUpgradeCheck):
		return "das heruntergeladene Programm funktioniert nicht; nichts wurde geändert."
	case errors.Is(err, kiosk.ErrUpgradeBusy):
		return "eine Aktualisierung läuft bereits."
	case errors.Is(err, kiosk.ErrUpToDate):
		return "schon aktuell."
	}
	return err.Error()
}

// lock toggles the lock (or sets it with "on"/"off").
func (bt *Bot) lock(ctx context.Context, m *models.Message, arg string) {
	on := !bt.k.Locked(ctx)
	switch arg {
	case "on", "an":
		on = true
	case "off", "aus":
		on = false
	}
	if err := bt.k.SetLocked(ctx, on); err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	if on {
		bt.reply(ctx, m, "🔒 Gesperrt: Weiter, Zurück, Pause, Streams, Neustarts, QR-Code und WLAN-Wechsel sind blockiert, ebenso Löschen des laufenden Eintrags. Playlist bearbeiten und Hochladen gehen weiter.\nEntsperren: /lock")
	} else {
		bt.reply(ctx, m, "🔓 Entsperrt.")
	}
}

func (bt *Bot) control(ctx context.Context, m *models.Message, fn func(context.Context) error, ok string) {
	if err := fn(ctx); err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	bt.reply(ctx, m, ok)
}

// qr shows the web UI's QR code on the screen (or hides it again) and sends
// it into the chat.
func (bt *Bot) qr(ctx context.Context, m *models.Message) {
	url := bt.k.AccessURL()
	if url == "" {
		bt.reply(ctx, m, "⚠️ Keine Adresse im lokalen Netz gefunden.")
		return
	}
	shown, err := bt.k.ToggleQR(ctx, kiosk.QRShowFor)
	if err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	if !shown {
		bt.reply(ctx, m, "QR-Code ausgeblendet.")
		return
	}
	caption := fmt.Sprintf("📺 Weboberfläche: %s\n\nAuf dem Bildschirm für %d s. Das Zertifikat ist selbst erstellt; der Browser warnt beim ersten Mal.",
		url, int(kiosk.QRShowFor.Seconds()))
	png, err := access.PNG(url, 8)
	if err != nil {
		bt.reply(ctx, m, caption)
		return
	}
	retry(ctx, func() error {
		_, err := bt.b.SendPhoto(ctx, &tg.SendPhotoParams{
			ChatID: m.Chat.ID, Caption: caption,
			Photo: &models.InputFileUpload{Filename: "qr.png", Data: bytes.NewReader(png)},
		})
		return err
	})
}

// screenshot sends a picture of what the screen shows right now.
func (bt *Bot) screenshot(ctx context.Context, m *models.Message) {
	jpg, err := bt.k.Screenshot(ctx)
	if err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	caption, _, _ := strings.Cut(bt.statusText(ctx), "\n")
	retry(ctx, func() error {
		_, err := bt.b.SendPhoto(ctx, &tg.SendPhotoParams{
			ChatID: m.Chat.ID, Caption: caption, ReplyParameters: &models.ReplyParameters{MessageID: m.ID},
			Photo: &models.InputFileUpload{Filename: "screen.jpg", Data: bytes.NewReader(jpg)},
		})
		return err
	})
}

// passwordMessageTTL is how long the new web password stays in the chat.
const passwordMessageTTL = 15 * time.Minute

// resetPassword sets a new random web password, for when it is forgotten.
// The message with it is deleted again after passwordMessageTTL.
func (bt *Bot) resetPassword(ctx context.Context, m *models.Message) {
	pw, err := bt.k.ResetWebPassword(ctx)
	if err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	bt.k.Logf(ctx, slog.LevelInfo, "web password reset via Telegram")
	text := "🔑 Neues Passwort für die Weboberfläche:\n\n" + pw +
		"\n\nAlle Browser wurden abgemeldet. Ändern unter Einstellungen.\n" +
		fmt.Sprintf("Diese Nachricht wird in %d Minuten gelöscht.", int(passwordMessageTTL.Minutes()))
	msg := bt.reply(ctx, m, text)
	if msg == nil {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(passwordMessageTTL):
			if _, err := bt.b.DeleteMessage(ctx, &tg.DeleteMessageParams{ChatID: msg.Chat.ID, MessageID: msg.ID}); err != nil {
				slog.Warn("delete password message", "err", err)
			}
		}
	}()
}

// restart restarts mpv (default) or, with "all", the whole service.
func (bt *Bot) restart(ctx context.Context, m *models.Message, arg string) {
	if arg == "all" {
		if err := bt.k.RestartService(ctx); err != nil {
			bt.reply(ctx, m, warn(err))
			return
		}
		bt.reply(ctx, m, "🔄 Dienst wird neu gestartet...")
		return
	}
	if err := bt.k.RestartPlayer(ctx); err != nil {
		bt.reply(ctx, m, warn(err))
		return
	}
	bt.reply(ctx, m, "🔄 Player wird neu gestartet, Playlist wird neu geladen.\n(Ganzen Dienst neu starten: /restart all)")
}

func (bt *Bot) showMenu(ctx context.Context, m *models.Message) {
	kb := &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{
			{{Text: "Upload Media"}, {Text: "Slides"}},
			{{Text: "Reload Playlist"}, {Text: "Next"}},
			{{Text: "Prev"}, {Text: "Pause/Resume"}},
			{{Text: "Status"}, {Text: "Help"}},
		},
		ResizeKeyboard: true,
	}
	bt.sendMarkup(ctx, m.Chat.ID, "Flimmerkiste", kb)
}

// --- uploads ----------------------------------------------------------------

var allowedDocExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true, ".heic": true, ".heif": true,
	".mp4": true, ".mov": true, ".mkv": true, ".webm": true,
}

func (bt *Bot) onMedia(ctx context.Context, m *models.Message) bool {
	var fileID, name string
	switch {
	case len(m.Photo) > 0:
		fileID, name = m.Photo[len(m.Photo)-1].FileID, "photo.jpg"
	case m.Video != nil:
		fileID, name = m.Video.FileID, m.Video.FileName
		if name == "" {
			name = "video.mp4"
		}
	case m.Animation != nil:
		fileID, name = m.Animation.FileID, m.Animation.FileName
		if name == "" {
			name = "animation.mp4"
		}
	case m.Document != nil:
		fileID, name = m.Document.FileID, m.Document.FileName
		if !allowedDocExt[strings.ToLower(extOf(name))] {
			bt.reply(ctx, m, "⚠️ Format nicht unterstützt: "+name)
			return true
		}
	default:
		return false
	}
	// Queue first, reply second: a failed reply must not lose the upload.
	j, err := bt.k.EnqueueTelegram(ctx, fileID, name, m.Caption, m.Chat.ID)
	if err != nil {
		bt.reply(ctx, m, "⚠️ Fehler: "+err.Error())
		return true
	}
	if pm := bt.reply(ctx, m, fmt.Sprintf("⏳ In Warteschlange (#%d)…", j.ID)); pm != nil {
		bt.k.SetJobProgressMessage(ctx, j.ID, m.Chat.ID, pm.ID)
	}
	return true
}

// watchJobs turns job events into edits of the "⏳" reply, at most every 5 s
// per job plus the final state.
func (bt *Bot) watchJobs(ctx context.Context) {
	events, cancel := bt.k.Hub.Subscribe()
	defer cancel()
	lastEdit := map[int64]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if e.Type != kiosk.EventJob {
				continue
			}
			j, err := bt.k.Job(ctx, e.JobID)
			if err != nil || j.TgChatID == 0 || j.TgProgressMsgID == 0 {
				continue
			}
			var text string
			switch j.State {
			case "running":
				if time.Since(lastEdit[j.ID]) < 5*time.Second {
					continue
				}
				text = fmt.Sprintf("⏳ Verarbeite… %d%%", j.Progress)
				if j.Phase == "download" {
					text = fmt.Sprintf("⬇️ Lade herunter… %d%%", j.Progress)
				}
			case "done":
				m, _ := bt.k.Get(ctx, j.MediaID.Int64)
				text = fmt.Sprintf("✅ Hinzugefügt: %s (Position %d)", m.Title, m.Position)
				delete(lastEdit, j.ID)
			case "failed":
				text = "⚠️ " + j.Error
				delete(lastEdit, j.ID)
			case "cancelled":
				text = "🚫 Abgebrochen"
				delete(lastEdit, j.ID)
			default:
				continue
			}
			if j.State == "running" {
				lastEdit[j.ID] = time.Now()
			}
			bt.edit(ctx, j.TgChatID, int(j.TgProgressMsgID), text, nil, j.State != "running")
		}
	}
}

// onDownloadURL checks a link with yt-dlp and queues it with the format
// chosen for the CRT. Progress is reported by watchJobs like for uploads.
func (bt *Bot) onDownloadURL(ctx context.Context, m *models.Message, url string) {
	pm := bt.reply(ctx, m, "🔎 Prüfe Link…")
	edit := func(text string) {
		if pm != nil {
			bt.edit(ctx, m.Chat.ID, pm.ID, text, nil, true)
		}
	}
	info, err := bt.k.ProbeURL(ctx, url)
	switch {
	case errors.Is(err, kiosk.ErrPlaylist):
		edit("⚠️ Playlists werden nicht unterstützt, bitte ein einzelnes Video schicken.")
		return
	case err != nil:
		edit(warn(err))
		return
	case info.Live:
		edit("⚠️ Livestreams lassen sich nicht herunterladen. In der Weboberfläche als Stream hinzufügen.")
		return
	}
	size := int64(0)
	for _, f := range info.Formats {
		if f.Spec == info.Auto {
			size = f.Size
		}
	}
	j, err := bt.k.EnqueueURL(ctx, info.URL, info.Auto, info.Title, kiosk.Caption{}, size)
	if err != nil {
		edit(warn(err))
		return
	}
	edit(fmt.Sprintf("⏳ In Warteschlange (#%d): %s", j.ID, info.Title))
	if pm != nil {
		bt.k.SetJobProgressMessage(ctx, j.ID, m.Chat.ID, pm.ID)
	}
}

// fetch downloads a Telegram file (Bot API limit: 20 MB).
func (bt *Bot) fetch(ctx context.Context, fileID, dst string) error {
	f, err := bt.b.GetFile(ctx, &tg.GetFileParams{FileID: fileID})
	if err != nil {
		if strings.Contains(err.Error(), "too big") {
			return errors.New("Datei größer als 20 MB – bitte über die Weboberfläche hochladen")
		}
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bt.b.FileDownloadLink(f), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// --- sending with rate-limit handling ---------------------------------------

func (bt *Bot) reply(ctx context.Context, m *models.Message, text string) *models.Message {
	return bt.sendTo(ctx, m.Chat.ID, text, nil, &models.ReplyParameters{MessageID: m.ID})
}

func (bt *Bot) send(ctx context.Context, text string, markup models.ReplyMarkup) *models.Message {
	return bt.sendTo(ctx, bt.groupID, text, markup, nil)
}

func (bt *Bot) sendMarkup(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) *models.Message {
	return bt.sendTo(ctx, chatID, text, markup, nil)
}

func (bt *Bot) sendTo(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup, rp *models.ReplyParameters) *models.Message {
	var msg *models.Message
	retry(ctx, func() error {
		var err error
		msg, err = bt.b.SendMessage(ctx, &tg.SendMessageParams{ChatID: chatID, Text: text, ReplyMarkup: markup, ReplyParameters: rp})
		return err
	})
	return msg
}

// edit changes a message; must = false drops the edit when rate-limited.
func (bt *Bot) edit(ctx context.Context, chatID int64, msgID int, text string, markup models.ReplyMarkup, must bool) {
	do := func() error {
		_, err := bt.b.EditMessageText(ctx, &tg.EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text, ReplyMarkup: markup})
		if err != nil && strings.Contains(err.Error(), "message is not modified") {
			return nil
		}
		return err
	}
	if must {
		retry(ctx, do)
	} else if err := do(); err != nil {
		slog.Debug("edit dropped", "err", err)
	}
}

// retry waits out Telegram's flood limit (retry_after) up to three times.
func retry(ctx context.Context, fn func() error) {
	for i := 0; i < 4; i++ {
		err := fn()
		var tm *tg.TooManyRequestsError
		if !errors.As(err, &tm) {
			if err != nil {
				slog.Error("telegram send", "err", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(tm.RetryAfter+1) * time.Second):
		}
	}
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func extOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}
