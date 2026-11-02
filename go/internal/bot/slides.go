package bot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/rnkch/flimmerkiste/internal/core"
	"github.com/rnkch/flimmerkiste/internal/store/db"
	"github.com/rnkch/flimmerkiste/internal/sysinfo"
)

// Callback data: "page:<n>", "slide:<id>", "act:<action>:<id>".

// showSlides sends the playlist page, or edits msgID when it is not 0.
func (bt *Bot) showSlides(ctx context.Context, chatID int64, msgID int, page int) {
	list, err := bt.k.List(ctx)
	if err != nil {
		bt.sendMarkup(ctx, chatID, warn(err), nil)
		return
	}
	if len(list) == 0 {
		bt.show(ctx, chatID, msgID, "📺 Das Schaufenster ist aktuell leer.", nil)
		return
	}
	pages := (len(list) + pageSize - 1) / pageSize
	page = min(max(page, 1), pages)
	var rows [][]models.InlineKeyboardButton
	for _, m := range list[(page-1)*pageSize : min(page*pageSize, len(list))] {
		id := strconv.FormatInt(m.ID, 10)
		icon := map[string]string{"image": "🖼", "video": "🎞", "stream": "📡"}[m.Kind]
		if !core.Enabled(m) {
			icon = "⏸" // stays in the list, is not played
		}
		rows = append(rows, []models.InlineKeyboardButton{
			{
				Text:         fmt.Sprintf("%d. %s %s", m.Position, icon, short(m.Title, 14)),
				CallbackData: "slide:" + id,
			},
			{Text: "Up", CallbackData: "act:up:" + id},
			{Text: "Down", CallbackData: "act:down:" + id},
		})
	}
	var nav []models.InlineKeyboardButton
	if page > 1 {
		nav = append(
			nav,
			models.InlineKeyboardButton{Text: "Prev", CallbackData: fmt.Sprintf("page:%d", page-1)},
		)
	}
	if page < pages {
		nav = append(
			nav,
			models.InlineKeyboardButton{Text: "Next", CallbackData: fmt.Sprintf("page:%d", page+1)},
		)
	}
	if nav != nil {
		rows = append(rows, nav)
	}
	text := fmt.Sprintf("PLAYLIST (%d Einträge, Seite %d/%d)", len(list), page, pages)
	if off := len(list) - countEnabled(list); off > 0 {
		text = fmt.Sprintf(
			"PLAYLIST (%d Einträge, davon %d ausgeschaltet ⏸, Seite %d/%d)",
			len(list), off, page, pages,
		)
	}
	bt.show(ctx, chatID, msgID, text, &models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func countEnabled(list []db.Media) (n int) {
	for _, m := range list {
		if core.Enabled(m) {
			n++
		}
	}
	return n
}

func (bt *Bot) show(
	ctx context.Context,
	chatID int64,
	msgID int,
	text string,
	markup models.ReplyMarkup,
) {
	if msgID != 0 {
		bt.edit(ctx, chatID, msgID, text, markup, true)
	} else {
		bt.sendMarkup(ctx, chatID, text, markup)
	}
}

func (bt *Bot) pageOf(ctx context.Context, id int64) int {
	m, err := bt.k.Get(ctx, id)
	if err != nil {
		return 1
	}
	return int(m.Position-1)/pageSize + 1
}

func (bt *Bot) onCallback(ctx context.Context, q *models.CallbackQuery) {
	msg := q.Message.Message
	if msg == nil || msg.Chat.ID != bt.groupID {
		return
	}
	answer := func(text string, alert bool) {
		bt.b.AnswerCallbackQuery(
			ctx,
			&tg.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: text, ShowAlert: alert},
		)
	}
	parts := strings.Split(q.Data, ":")
	chatID, msgID := msg.Chat.ID, msg.ID

	if parts[0] == "page" && len(parts) == 2 {
		answer("", false)
		p, _ := strconv.Atoi(parts[1])
		bt.showSlides(ctx, chatID, msgID, p)
		return
	}
	if parts[0] == "slide" && len(parts) == 2 {
		answer("", false)
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		bt.showActions(ctx, chatID, msgID, id)
		return
	}
	if parts[0] != "act" || len(parts) < 2 {
		answer("", false)
		return
	}
	var id int64
	if len(parts) > 2 {
		id, _ = strconv.ParseInt(parts[2], 10, 64)
	}
	switch parts[1] {
	case "up", "down":
		delta := -1
		if parts[1] == "down" {
			delta = 1
		}
		err := bt.k.MoveBy(ctx, id, delta)
		if errors.Is(err, core.ErrCannotMove) {
			answer("Geht nicht weiter", true)
			return
		}
		if err != nil {
			answer(warn(err), true)
			return
		}
		answer("", false)
		bt.showSlides(ctx, chatID, msgID, bt.pageOf(ctx, id))
	case "rename", "duration", "position":
		answer("", false)
		m, err := bt.k.Get(ctx, id)
		if err != nil {
			bt.sendMarkup(ctx, chatID, "⚠️ Nicht gefunden.", nil)
			return
		}
		bt.mu.Lock()
		bt.pending[q.From.ID] = pendingInput{action: parts[1], mediaID: id}
		bt.mu.Unlock()
		prompt := map[string]string{
			"rename":   fmt.Sprintf("Neuer Name für \"%s\":", m.Title),
			"duration": fmt.Sprintf("Aktuell: %ds. Neue Anzeigedauer in Sekunden:", m.ImageDurationS),
			"position": fmt.Sprintf("Aktuelle Position: %d\nNeue Position (1-99, 99 = Ende):", m.Position),
		}[parts[1]]
		bt.sendMarkup(ctx, chatID, prompt, &models.ForceReply{ForceReply: true, Selective: true})
	case "toggle":
		m, err := bt.k.Get(ctx, id)
		if err == nil {
			err = bt.k.SetEnabled(ctx, id, !core.Enabled(m))
		}
		if err != nil {
			answer(warn(err), true)
			return
		}
		if core.Enabled(m) { // it was on: now off
			answer("Ausgeschaltet: wird übersprungen", false)
		} else {
			answer("Eingeschaltet", false)
		}
		bt.showActions(ctx, chatID, msgID, id)
	case "delete":
		if err := bt.k.Delete(ctx, id); err != nil {
			answer(warn(err), true)
			return
		}
		answer("Gelöscht", false)
		bt.showSlides(ctx, chatID, msgID, 1)
	case "back":
		answer("", false)
		bt.showSlides(ctx, chatID, msgID, bt.pageOf(ctx, id))
	default:
		answer("", false)
	}
}

func (bt *Bot) showActions(ctx context.Context, chatID int64, msgID int, id int64) {
	m, err := bt.k.Get(ctx, id)
	if err != nil {
		bt.showSlides(ctx, chatID, msgID, 1)
		return
	}
	sid := strconv.FormatInt(id, 10)
	rows := [][]models.InlineKeyboardButton{
		{{Text: "Rename", CallbackData: "act:rename:" + sid}},
		{{Text: "Disable", CallbackData: "act:toggle:" + sid}},
		{
			{Text: "Delete", CallbackData: "act:delete:" + sid},
			{Text: "Position", CallbackData: "act:position:" + sid},
		},
		{{Text: "Back to slides", CallbackData: "act:back:" + sid}},
	}
	if !core.Enabled(m) {
		rows[1][0].Text = "Enable"
	}
	info := fmt.Sprintf("✏️ \"%s\"\nPosition %d · ", m.Title, m.Position)
	if !core.Enabled(m) {
		info = fmt.Sprintf(
			"⏸ \"%s\" (ausgeschaltet, wird übersprungen)\nPosition %d · ",
			m.Title,
			m.Position,
		)
	}
	switch m.Kind {
	case "image":
		rows[0] = append(
			rows[0],
			models.InlineKeyboardButton{Text: "Duration", CallbackData: "act:duration:" + sid},
		)
		info += fmt.Sprintf("Bild, %ds", m.ImageDurationS)
	case "stream":
		info += "Stream, " + m.URL
	default:
		info += "Video, " + (time.Duration(m.DurationS) * time.Second).String()
	}
	bt.edit(ctx, chatID, msgID, info, &models.InlineKeyboardMarkup{InlineKeyboard: rows}, true)
}

// onReplyInput handles the answer to a Rename/Duration/Position prompt.
func (bt *Bot) onReplyInput(ctx context.Context, m *models.Message, text string) bool {
	if m.From == nil {
		return false
	}
	bt.mu.Lock()
	p, ok := bt.pending[m.From.ID]
	delete(bt.pending, m.From.ID)
	bt.mu.Unlock()
	if !ok {
		return false
	}
	var err error
	var done string
	switch p.action {
	case "rename":
		err = bt.k.Rename(ctx, p.mediaID, text)
		done = "✅ Umbenannt in: " + text
	case "duration":
		n, convErr := strconv.Atoi(text)
		if convErr != nil || n <= 0 {
			bt.reply(ctx, m, "⚠️ Bitte eine positive Zahl eingeben.")
			return true
		}
		err = bt.k.SetImageDuration(ctx, p.mediaID, n)
		done = fmt.Sprintf("✅ Anzeigedauer: %ds", n)
	case "position":
		n, convErr := strconv.Atoi(text)
		if convErr != nil || n >= 99 {
			n = 1 << 30 // end
		}
		err = bt.k.Move(ctx, p.mediaID, n)
		if mm, e := bt.k.Get(ctx, p.mediaID); e == nil {
			done = fmt.Sprintf("✅ Verschoben auf Position %d.", mm.Position)
		}
	}
	if err != nil {
		bt.reply(ctx, m, warn(err))
		return true
	}
	bt.reply(ctx, m, done)
	bt.showSlides(ctx, m.Chat.ID, 0, bt.pageOf(ctx, p.mediaID))
	return true
}

func (bt *Bot) statusText(ctx context.Context) string {
	ps := bt.k.PlayerState()
	si := sysinfo.Read(ctx, bt.k.Config().MediaDir)
	var b strings.Builder
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "📺 %s · Version %s\n", host, core.Version())
	if bt.k.Locked(ctx) {
		b.WriteString("🔒 Gesperrt (entsperren: /lock)\n")
	}
	if url := bt.k.AccessURL(); url != "" {
		fmt.Fprintf(&b, "🌐 %s\n", url)
	}
	switch {
	case ps.Streaming != "":
		fmt.Fprintf(&b, "▶️ Stream: %s\n", ps.Streaming)
	case ps.Path != "":
		title := ps.Path
		pos, dur := ps.TimePos, ps.Duration
		if list, err := bt.k.List(ctx); err == nil {
			for _, m := range list {
				if bt.k.MediaPath(m) == ps.Path {
					title = fmt.Sprintf("%d. %s", m.Position, m.Title)
					pos, dur = bt.k.Progress(m, ps)
				}
			}
		}
		state := "▶️"
		if ps.Pause {
			state = "⏸"
		}
		fmt.Fprintf(&b, "%s %s", state, title)
		if dur > 0 {
			fmt.Fprintf(&b, " (%s / %s)", fmtDur(pos), fmtDur(dur))
		}
		b.WriteString("\n")
	default:
		b.WriteString("⏹ Nichts wird abgespielt\n")
	}
	if jobs, err := bt.k.ActiveJobs(ctx); err == nil && len(jobs) > 0 {
		fmt.Fprintf(&b, "⏳ Warteschlange: %d\n", len(jobs))
	}
	if si.DiskTotal > 0 {
		fmt.Fprintf(
			&b,
			"💾 Frei: %.1f GB von %.1f GB\n",
			float64(si.DiskFree)/1e9,
			float64(si.DiskTotal)/1e9,
		)
	}
	if si.TempC > 0 {
		fmt.Fprintf(&b, "🌡 CPU: %.0f °C\n", si.TempC)
	}
	switch {
	case si.UnderVoltageNow():
		b.WriteString("⚠️ Jetzt: Unterspannung – Netzteil prüfen\n")
	case si.UnderVoltageEarlier():
		b.WriteString("⚠️ Seit Neustart: Unterspannung – Netzteil prüfen\n")
	}
	if si.SlowedNow() {
		b.WriteString("⚠️ Jetzt: gedrosselt (warm)\n")
	}
	if si.WarmEarlier() {
		b.WriteString("ℹ️ Seit Neustart: zeitweise warm (normal unter Last)\n")
	}
	if si.Uptime > 0 {
		fmt.Fprintf(&b, "⏱ Uptime: %s\n", si.Uptime.Truncate(time.Minute))
	}
	return strings.TrimSpace(b.String())
}

func fmtDur(sec float64) string {
	s := int(sec)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func short(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
