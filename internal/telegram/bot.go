package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jsolteam/jsol-uptime-bot/internal/charts"
	"github.com/jsolteam/jsol-uptime-bot/internal/config"
	"github.com/jsolteam/jsol-uptime-bot/internal/storage"
)

type Bot struct {
	api              *tgbotapi.BotAPI
	token            string
	client           *http.Client
	apiMu            sync.RWMutex
	cfg              config.Config
	store            *storage.Store
	notifyMu         sync.Mutex
	notifyWindow     time.Time
	notifyCount      int
	notifySuppressed int
	notifyLast       map[string]time.Time
}

func New(token string, c config.Config, s *storage.Store) (*Bot, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("пустой токен Telegram")
	}
	client := &http.Client{Timeout: time.Duration(c.Monitoring.Timeout) + 2*time.Second}
	return &Bot{token: token, client: client, cfg: c, store: s, notifyLast: map[string]time.Time{}}, nil
}

func (b *Bot) apiClient() *tgbotapi.BotAPI {
	b.apiMu.RLock()
	defer b.apiMu.RUnlock()
	return b.api
}

func (b *Bot) ensureAPI() error {
	if b.apiClient() != nil {
		return nil
	}
	api, err := tgbotapi.NewBotAPIWithClient(b.token, tgbotapi.APIEndpoint, b.client)
	if err != nil {
		return err
	}
	b.apiMu.Lock()
	b.api = api
	b.apiMu.Unlock()
	return nil
}
func (b *Bot) IsAdmin(id int64) bool {
	for _, v := range b.cfg.Admins {
		if v == id {
			return true
		}
	}
	return false
}
func (b *Bot) allowed(ctx context.Context, id int64) bool {
	if b.IsAdmin(id) {
		return true
	}
	ok, err := b.store.Allowed(ctx, id)
	return err == nil && ok
}
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 1
	consecutiveErrors := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := b.ensureAPI(); err != nil {
			consecutiveErrors++
			if consecutiveErrors >= b.cfg.Monitoring.FailureThreshold {
				return fmt.Errorf("Telegram polling unavailable after %d initialization errors: %w", consecutiveErrors, err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		updates, err := b.apiClient().GetUpdates(u)
		if err != nil {
			if isPollingConflict(err) {
				return fmt.Errorf("Telegram polling conflict: %w", err)
			}
			consecutiveErrors++
			if consecutiveErrors >= b.cfg.Monitoring.FailureThreshold {
				return fmt.Errorf("Telegram polling unavailable after %d errors: %w", consecutiveErrors, err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		consecutiveErrors = 0
		for _, update := range updates {
			if update.UpdateID >= u.Offset {
				u.Offset = update.UpdateID + 1
			}
			if ctx.Err() != nil {
				return nil
			}
			if update.Message != nil {
				b.message(ctx, update.Message)
			}
			if update.CallbackQuery != nil {
				b.callback(ctx, update.CallbackQuery)
			}
		}
	}
}

func isPollingConflict(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == 409 {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "conflict") || strings.Contains(message, "409")
}
func (b *Bot) message(ctx context.Context, m *tgbotapi.Message) {
	if !m.IsCommand() {
		return
	}
	api := b.apiClient()
	if api == nil {
		return
	}
	id := m.From.ID
	if !b.allowed(ctx, id) {
		if m.Command() == "start" {
			accepted, err := b.store.AcceptInvite(ctx, id, m.From.UserName)
			if err == nil && accepted {
				b.sendMenu(m.Chat.ID, "✅ Доступ выдан. Выберите раздел мониторинга.")
				return
			}
		}
		b.send(m.Chat.ID, "Доступ закрыт. Попросите администратора пригласить ваш Telegram username.")
		return
	}
	if m.Command() == "invite" {
		if !b.IsAdmin(id) {
			b.send(m.Chat.ID, "Недостаточно прав.")
			return
		}
		args := strings.Fields(m.CommandArguments())
		if len(args) != 1 {
			b.sendMenu(m.Chat.ID, "Использование: /invite @username")
			return
		}
		if err := b.store.Invite(ctx, args[0], id); err != nil {
			b.sendMenu(m.Chat.ID, "Не удалось создать приглашение: "+err.Error())
			return
		}
		b.sendMenu(m.Chat.ID, "✅ Приглашение создано. Пользователь должен открыть бота через /start.")
		return
	}
	msg := tgbotapi.NewMessage(m.Chat.ID, "📡 JSOL Uptime\n\nВыберите раздел мониторинга.")
	msg.ReplyMarkup = menu(b.IsAdmin(id))
	_, _ = api.Send(msg)
}
func menu(admin bool) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🖥 Серверы", "servers"), tgbotapi.NewInlineKeyboardButtonData("📈 Графики", "charts")},
		{tgbotapi.NewInlineKeyboardButtonData("📋 Журнал событий", "events"), tgbotapi.NewInlineKeyboardButtonData("🗓 Сводка за сутки", "summary")},
	}
	if admin {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("👥 Пользователи", "users")})
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func navigation(back string) []tgbotapi.InlineKeyboardButton {
	return []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("⬅️ Назад", back),
		tgbotapi.NewInlineKeyboardButtonData("🏠 В меню", "menu"),
	}
}

func (b *Bot) callback(ctx context.Context, q *tgbotapi.CallbackQuery) {
	if q == nil {
		return
	}
	api := b.apiClient()
	if api == nil {
		return
	}
	if !b.allowed(ctx, q.From.ID) {
		_, _ = api.Request(tgbotapi.NewCallback(q.ID, "Доступ закрыт"))
		return
	}
	if q.Message == nil {
		_, _ = api.Request(tgbotapi.NewCallback(q.ID, "Сообщение недоступно"))
		return
	}
	_, _ = api.Request(tgbotapi.NewCallback(q.ID, ""))
	text := "Неизвестная команда. Откройте меню."
	rows := [][]tgbotapi.InlineKeyboardButton{}
	switch q.Data {
	case "menu":
		text = "📡 JSOL Uptime\n\nВыберите раздел мониторинга."
		rows = menu(b.IsAdmin(q.From.ID)).InlineKeyboard
	case "servers":
		text = "🖥 Серверы\nВыберите узел, чтобы открыть подробную карточку."
		for _, s := range b.cfg.Servers {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData(b.serverLabel(s), "server:"+s.ID)})
		}
		rows = append(rows, navigation("menu"))
	case "charts":
		text = "📈 Графики\n\nВыберите сервер."
		for _, s := range b.cfg.Servers {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData(b.serverLabel(s), "chartmenu:"+s.ID)})
		}
		rows = append(rows, navigation("menu"))
	case "events":
		text = b.events()
		rows = append(rows, navigation("menu"))
	case "summary":
		text = b.summary()
		rows = append(rows, navigation("menu"))
	case "users":
		if !b.IsAdmin(q.From.ID) {
			text = "Недостаточно прав."
		} else {
			text, rows = b.users(ctx)
			rows = append(rows, navigation("menu"))
		}
	default:
		if strings.HasPrefix(q.Data, "server:") {
			id := strings.TrimPrefix(q.Data, "server:")
			text = b.server(id)
			rows = append(rows,
				[]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("🔄 Обновить", "server:"+id), tgbotapi.NewInlineKeyboardButtonData("📈 Графики", "chartmenu:"+id)},
				navigation("servers"),
			)
		} else if strings.HasPrefix(q.Data, "chartmenu:") {
			id := strings.TrimPrefix(q.Data, "chartmenu:")
			text = "📈 " + b.serverName(id) + "\n\nВыберите период:"
			rows = [][]tgbotapi.InlineKeyboardButton{
				{tgbotapi.NewInlineKeyboardButtonData("1 час", "chartperiod:"+id+":1"), tgbotapi.NewInlineKeyboardButtonData("6 часов", "chartperiod:"+id+":6")},
				{tgbotapi.NewInlineKeyboardButtonData("24 часа", "chartperiod:"+id+":24"), tgbotapi.NewInlineKeyboardButtonData("7 дней", "chartperiod:"+id+":168")},
				navigation("charts"),
			}
		} else if strings.HasPrefix(q.Data, "chartperiod:") {
			parts := strings.Split(q.Data, ":")
			if len(parts) == 3 {
				text = "📊 " + b.serverName(parts[1]) + "\nПериод: " + periodLabel(parts[2]) + "\n\nВыберите показатель:"
				rows = [][]tgbotapi.InlineKeyboardButton{
					{tgbotapi.NewInlineKeyboardButtonData("CPU", "chart:"+parts[1]+":"+parts[2]+":cpu"), tgbotapi.NewInlineKeyboardButtonData("RAM", "chart:"+parts[1]+":"+parts[2]+":ram")},
					{tgbotapi.NewInlineKeyboardButtonData("Доступность", "chart:"+parts[1]+":"+parts[2]+":availability")},
					navigation("chartmenu:" + parts[1]),
				}
			}
		} else if strings.HasPrefix(q.Data, "chart:") {
			parts := strings.Split(q.Data, ":")
			if len(parts) == 4 {
				b.sendChart(ctx, q.Message.Chat.ID, parts[1], parts[2], parts[3])
				text = "📈 График отправлен\n" + b.serverName(parts[1]) + " · " + periodLabel(parts[2])
				rows = [][]tgbotapi.InlineKeyboardButton{navigation("chartperiod:" + parts[1] + ":" + parts[2])}
			}
		} else if strings.HasPrefix(q.Data, "disable:") {
			if !b.IsAdmin(q.From.ID) {
				text = "Недостаточно прав."
			} else {
				n, e := strconv.ParseInt(strings.TrimPrefix(q.Data, "disable:"), 10, 64)
				if e != nil {
					text = "Некорректный ID"
				} else {
					rows = [][]tgbotapi.InlineKeyboardButton{
						{tgbotapi.NewInlineKeyboardButtonData("Отключить доступ", "confirm-disable:"+strconv.FormatInt(n, 10))},
						navigation("users"),
					}
					text = fmt.Sprintf("⚠️ Отключить доступ пользователя %d?", n)
				}
			}
		} else if strings.HasPrefix(q.Data, "confirm-disable:") {
			if !b.IsAdmin(q.From.ID) {
				text = "Недостаточно прав."
			} else {
				n, e := strconv.ParseInt(strings.TrimPrefix(q.Data, "confirm-disable:"), 10, 64)
				if e != nil || n == 0 {
					text = "Некорректный ID"
				} else if e = b.store.DisableUser(ctx, n); e != nil {
					text = "Не удалось изменить доступ"
				} else {
					text = "✅ Доступ отключён."
					rows = append(rows, navigation("users"))
				}
			}
		}
	}
	b.editScreen(api, q.Message, text, rows)
}

func (b *Bot) editScreen(api *tgbotapi.BotAPI, message *tgbotapi.Message, text string, rows [][]tgbotapi.InlineKeyboardButton) {
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	var edit tgbotapi.Chattable
	if len(message.Photo) > 0 {
		config := tgbotapi.NewEditMessageCaption(message.Chat.ID, message.MessageID, text)
		config.ReplyMarkup = &markup
		edit = config
	} else {
		edit = tgbotapi.NewEditMessageTextAndMarkup(message.Chat.ID, message.MessageID, text, markup)
	}
	if _, err := api.Send(edit); err != nil && !strings.Contains(err.Error(), "message is not modified") {
		fallback := tgbotapi.NewMessage(message.Chat.ID, text)
		fallback.ReplyMarkup = markup
		if _, sendErr := api.Send(fallback); sendErr != nil {
			slog.Warn("не удалось обновить экран бота", "error", sendErr)
		}
	}
}

func periodLabel(value string) string {
	labels := map[string]string{"1": "1 час", "6": "6 часов", "24": "24 часа", "168": "7 дней"}
	if label, ok := labels[value]; ok {
		return label
	}
	return value + " ч"
}

func serverStatus(status string) string {
	switch status {
	case "UP":
		return "🟢 Доступен"
	case "DOWN":
		return "🔴 Недоступен"
	default:
		return "⚪ Неизвестно"
	}
}

func internetStatus(status string) string {
	switch status {
	case "INTERNET_OK":
		return "🟢 Есть доступ"
	case "NO_INTERNET":
		return "🔴 Нет доступа"
	default:
		return "⚪ Неизвестно"
	}
}

func agentStatus(status string) string {
	switch status {
	case "AVAILABLE":
		return "🟢 Работает"
	case "AGENT_UNAVAILABLE":
		return "🔴 Нет ответа"
	default:
		return "⚪ Неизвестно"
	}
}

func pingStatus(status string) string {
	switch status {
	case "LOCAL":
		return "локальный узел"
	case "REACHABLE":
		return "🟢 отвечает"
	case "NO_REPLY":
		return "🔴 нет ответа"
	default:
		return "⚪ неизвестно"
	}
}

func uptimeLabel(seconds uint64) string {
	days := seconds / 86400
	hours := seconds % 86400 / 3600
	minutes := seconds % 3600 / 60
	if days > 0 {
		return fmt.Sprintf("%d д %d ч", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%d ч %d мин", hours, minutes)
	}
	return fmt.Sprintf("%d мин", minutes)
}
func (b *Bot) send(chat int64, text string) {
	if api := b.apiClient(); api != nil {
		_, _ = api.Send(tgbotapi.NewMessage(chat, text))
	}
}

func (b *Bot) sendMenu(chat int64, text string) {
	if api := b.apiClient(); api != nil {
		message := tgbotapi.NewMessage(chat, text)
		message.ReplyMarkup = menu(b.IsAdmin(chat))
		_, _ = api.Send(message)
	}
}
func (b *Bot) Notify(text string) {
	api := b.apiClient()
	if api == nil {
		return
	}
	b.notifyMu.Lock()
	now := time.Now()
	prefix := ""
	if now.Sub(b.notifyWindow) >= time.Minute {
		if b.notifySuppressed > 0 {
			prefix = fmt.Sprintf("За последнюю минуту скрыто уведомлений: %d.\n", b.notifySuppressed)
		}
		b.notifyWindow = now
		b.notifyCount = 0
		b.notifySuppressed = 0
	}
	if last := b.notifyLast[text]; now.Sub(last) < 2*time.Minute {
		b.notifyMu.Unlock()
		return
	}
	if b.notifyCount >= 10 {
		b.notifySuppressed++
		b.notifyMu.Unlock()
		return
	}
	b.notifyLast[text] = now
	b.notifyCount++
	if len(b.notifyLast) > 500 {
		for key, last := range b.notifyLast {
			if now.Sub(last) > 10*time.Minute {
				delete(b.notifyLast, key)
			}
		}
	}
	if b.notifySuppressed > 0 {
		prefix = fmt.Sprintf("Сетевые события ограничены: скрыто уведомлений %d.\n", b.notifySuppressed)
		b.notifySuppressed = 0
	}
	text = prefix + text
	b.notifyMu.Unlock()
	for _, id := range b.cfg.Admins {
		message := tgbotapi.NewMessage(id, text)
		message.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup([]tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📡 Открыть меню", "menu"),
		})
		_, _ = api.Send(message)
	}
}
func (b *Bot) RunDailyReports(ctx context.Context, claim func(context.Context, string) (bool, error)) {
	loc, err := time.LoadLocation(b.cfg.Telegram.Timezone)
	if err != nil {
		loc = time.UTC
	}
	hour, minute := 9, 0
	if b.cfg.Telegram.DailyReportTime != "" {
		if parsed, e := time.Parse("15:04", b.cfg.Telegram.DailyReportTime); e == nil {
			hour, minute = parsed.Hour(), parsed.Minute()
		} else {
			return
		}
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	sentDate := ""
	for {
		now := time.Now().In(loc)
		date := now.Format("2006-01-02")
		if b.apiClient() != nil && sentDate != date && (now.Hour() > hour || now.Hour() == hour && now.Minute() >= minute) {
			ok, e := claim(ctx, date)
			if e == nil && ok {
				b.sendDailyReport(ctx, loc)
			}
			if e == nil {
				sentDate = date
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (b *Bot) sendDailyReport(ctx context.Context, loc *time.Location) {
	api := b.apiClient()
	if api == nil {
		return
	}
	var lines []string
	lines = append(lines, "🗓 Сводка за сутки · "+time.Now().In(loc).Format("02.01.2006"))
	for _, server := range b.cfg.Servers {
		var status, internet string
		var cpu, ram, total float64
		err := b.store.DB.QueryRowContext(ctx, `SELECT status,internet,cpu,ram_used,ram_total FROM samples WHERE server_id=? ORDER BY checked_at DESC LIMIT 1`, server.ID).Scan(&status, &internet, &cpu, &ram, &total)
		if err != nil {
			lines = append(lines, server.Name+": нет измерений")
			continue
		}
		ramPercent := float64(0)
		if total > 0 {
			ramPercent = ram / total * 100
		}
		var incidents int
		_ = b.store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE server_id=? AND occurred_at>=? AND kind IN ('down','recovery','internet','agent')`, server.ID, time.Now().Add(-24*time.Hour)).Scan(&incidents)
		lines = append(lines, fmt.Sprintf("🖥 %s\nДоступность: %s · Интернет: %s\nCPU %.1f%% · RAM %.1f%% · событий %d", server.Name, serverStatus(status), internetStatus(internet), cpu, ramPercent, incidents))
	}
	text := strings.Join(lines, "\n")
	ids, err := b.store.UserIDs(ctx)
	if err != nil {
		b.Notify(text)
		return
	}
	for _, id := range ids {
		message := tgbotapi.NewMessage(id, text)
		message.ReplyMarkup = menu(b.IsAdmin(id))
		if _, err := api.Send(message); err != nil {
			slog.Warn("ошибка отправки суточной сводки", "user_id", id, "error", err)
		}
	}
}
func (b *Bot) server(id string) string {
	valid := false
	name := id
	for _, s := range b.cfg.Servers {
		if s.ID == id {
			valid = true
			name = s.Name
		}
	}
	if !valid {
		return "Сервер не найден"
	}
	var status, internet, agent, ping string
	var cpu, ru, rt, du, dt float64
	var uptime uint64
	var at time.Time
	var errText string
	e := b.store.DB.QueryRow(`SELECT status,internet,agent_status,ping_status,cpu,ram_used,ram_total,disk_used,disk_total,checked_at,error,uptime FROM samples WHERE server_id=? ORDER BY checked_at DESC LIMIT 1`, id).Scan(&status, &internet, &agent, &ping, &cpu, &ru, &rt, &du, &dt, &at, &errText, &uptime)
	if e != nil {
		return "🖥 " + name + "\n\nПока нет измерений."
	}
	var details strings.Builder
	fmt.Fprintf(&details, "🖥 %s\n\nДоступность: %s\nПинг: %s\nИнтернет: %s\nАгент: %s\n", name, serverStatus(status), pingStatus(ping), internetStatus(internet), agentStatus(agent))
	if agent == "AVAILABLE" {
		fmt.Fprintf(&details, "\nCPU: %.1f%%\nRAM: %.2f / %.2f GiB (свободно %.2f GiB)\nДиск: %.2f / %.2f GiB (свободно %.2f GiB)\nАптайм: %s\n", cpu, ru/1073741824, rt/1073741824, max(0, rt-ru)/1073741824, du/1073741824, dt/1073741824, max(0, dt-du)/1073741824, uptimeLabel(uptime))
	} else {
		details.WriteString("\nМетрики: агент не передал данные\n")
	}
	fmt.Fprintf(&details, "\nПоследняя проверка: %s", at.Local().Format("02.01.2006 · 15:04:05"))
	if strings.TrimSpace(errText) != "" {
		errorText := strings.TrimSpace(errText)
		if len([]rune(errorText)) > 240 {
			errorText = string([]rune(errorText)[:240]) + "…"
		}
		fmt.Fprintf(&details, "\n\nПоследняя ошибка:\n%s", errorText)
	}
	return details.String()
}
func (b *Bot) sendChart(ctx context.Context, chat int64, serverID, hoursText, metric string) {
	api := b.apiClient()
	if api == nil {
		return
	}
	validServer := false
	name := serverID
	for _, server := range b.cfg.Servers {
		if server.ID == serverID {
			validServer = true
			name = server.Name
		}
	}
	hours, err := strconv.Atoi(hoursText)
	if !validServer || err != nil || (hours != 1 && hours != 6 && hours != 24 && hours != 168) || (metric != "cpu" && metric != "ram" && metric != "availability") {
		b.sendMenu(chat, "Некорректные параметры графика.")
		return
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	samples, err := b.store.Samples(ctx, serverID, since, 300)
	if err != nil {
		b.sendMenu(chat, "Не удалось прочитать историю измерений.")
		return
	}
	pngData, err := charts.Render(metric, samples)
	if err != nil {
		b.sendMenu(chat, "За выбранный период пока нет данных.")
		return
	}
	labels := map[string]string{"cpu": "CPU, %", "ram": "RAM, %", "availability": "Доступность, %"}
	caption := fmt.Sprintf("📈 %s\n%s · %s\n%s — %s", name, labels[metric], periodLabel(hoursText), since.Local().Format("02.01 15:04"), time.Now().Local().Format("02.01 15:04"))
	photo := tgbotapi.NewPhoto(chat, tgbotapi.FileBytes{Name: "chart.png", Bytes: pngData})
	photo.Caption = caption
	photo.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{
		{tgbotapi.NewInlineKeyboardButtonData("🔄 Другой график", "chartmenu:"+serverID)},
		navigation("chartperiod:" + serverID + ":" + hoursText),
	}}
	if _, err = api.Send(photo); err != nil {
		b.sendMenu(chat, "Не удалось отправить график.")
	}
}
func (b *Bot) events() string {
	rows, e := b.store.DB.Query(`SELECT occurred_at,server_id,kind,message FROM events ORDER BY id DESC LIMIT 10`)
	if e != nil {
		return "Не удалось прочитать журнал"
	}
	defer rows.Close()
	out := []string{"📋 Журнал событий", "Последние события:"}
	for rows.Next() {
		var t time.Time
		var id, k, m string
		if rows.Scan(&t, &id, &k, &m) == nil {
			icon := "ℹ️"
			switch k {
			case "down":
				icon = "🔴"
			case "recovery":
				icon = "🟢"
			case "internet", "agent":
				icon = "⚠️"
			case "leader":
				icon = "👑"
			}
			if id != "" {
				id = b.serverName(id)
			}
			out = append(out, fmt.Sprintf("%s %s · %s\n%s", icon, t.Local().Format("02.01 15:04"), id, m))
		}
	}
	if len(out) == 2 {
		return "📋 Журнал событий\n\nСобытий пока нет."
	}
	return strings.Join(out, "\n")
}
func (b *Bot) summary() string {
	var total, down int
	_ = b.store.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='DOWN'),0) FROM (SELECT server_id,status,MAX(checked_at) FROM samples GROUP BY server_id)`).Scan(&total, &down)
	return fmt.Sprintf("🗓 Сводка за сутки\n\nУзлов с данными: %d из %d\nНедоступны: %d\n\nПодробные показатели доступны в карточках серверов.", total, len(b.cfg.Servers), down)
}
func (b *Bot) users(ctx context.Context) (string, [][]tgbotapi.InlineKeyboardButton) {
	users, e := b.store.Users(ctx)
	if e != nil {
		return "Не удалось прочитать пользователей", nil
	}
	if len(users) == 0 {
		return "Список пуст. Пригласить: /invite @username", nil
	}
	var keys [][]tgbotapi.InlineKeyboardButton
	for _, line := range users {
		fields := strings.Fields(line)
		if len(fields) > 3 && fields[2] != "admin" && fields[3] == "активен" {
			keys = append(keys, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Отключить "+fields[0], "disable:"+fields[0])})
		}
	}
	return strings.Join(append(users, "Пригласить: /invite @username"), "\n"), keys
}

func (b *Bot) serverLabel(server config.Server) string {
	var status string
	if err := b.store.DB.QueryRow(`SELECT status FROM samples WHERE server_id=? ORDER BY checked_at DESC LIMIT 1`, server.ID).Scan(&status); err != nil {
		status = "UNKNOWN"
	}
	icon := "⚪"
	switch status {
	case "UP":
		icon = "🟢"
	case "DOWN":
		icon = "🔴"
	}
	return fmt.Sprintf("%s %s · приоритет %d", icon, server.Name, server.Priority)
}

func (b *Bot) serverName(id string) string {
	for _, server := range b.cfg.Servers {
		if server.ID == id {
			return server.Name
		}
	}
	return id
}
