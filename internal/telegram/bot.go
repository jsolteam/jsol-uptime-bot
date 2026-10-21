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
				b.send(m.Chat.ID, "Доступ выдан. Откройте /start ещё раз.")
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
			b.send(m.Chat.ID, "Использование: /invite @username")
			return
		}
		if err := b.store.Invite(ctx, args[0], id); err != nil {
			b.send(m.Chat.ID, "Не удалось создать приглашение: "+err.Error())
			return
		}
		b.send(m.Chat.ID, "Приглашение создано. Пользователь должен открыть бота через /start.")
		return
	}
	msg := tgbotapi.NewMessage(m.Chat.ID, "Мониторинг серверов JSOL Team")
	msg.ReplyMarkup = menu(b.IsAdmin(id))
	_, _ = api.Send(msg)
}
func menu(admin bool) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{{tgbotapi.NewInlineKeyboardButtonData("Серверы", "servers")}, {tgbotapi.NewInlineKeyboardButtonData("Графики", "charts")}}
	if admin {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Пользователи", "users")})
	}
	rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Журнал событий", "events")}, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Сводка за сутки", "summary")})
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
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
	text := "Неизвестная команда"
	rows := [][]tgbotapi.InlineKeyboardButton{}
	switch q.Data {
	case "servers":
		text = "Серверы:"
		for _, s := range b.cfg.Servers {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData(b.serverLabel(s), "server:"+s.ID)})
		}
	case "charts":
		text = "Выберите сервер:"
		for _, s := range b.cfg.Servers {
			rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData(s.Name, "chartmenu:"+s.ID)})
		}
	case "events":
		text = b.events()
	case "summary":
		text = b.summary()
	case "users":
		if !b.IsAdmin(q.From.ID) {
			text = "Недостаточно прав."
		} else {
			text, rows = b.users(ctx)
		}
	default:
		if strings.HasPrefix(q.Data, "server:") {
			id := strings.TrimPrefix(q.Data, "server:")
			text = b.server(id)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Обновить", "server:"+id), tgbotapi.NewInlineKeyboardButtonData("Графики", "chartmenu:"+id)}, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("К списку", "servers")})
		}
		if strings.HasPrefix(q.Data, "chartmenu:") {
			id := strings.TrimPrefix(q.Data, "chartmenu:")
			text = "Выберите период:"
			rows = [][]tgbotapi.InlineKeyboardButton{{tgbotapi.NewInlineKeyboardButtonData("1 час", "chartperiod:"+id+":1")}, {tgbotapi.NewInlineKeyboardButtonData("6 часов", "chartperiod:"+id+":6")}, {tgbotapi.NewInlineKeyboardButtonData("24 часа", "chartperiod:"+id+":24")}, {tgbotapi.NewInlineKeyboardButtonData("7 дней", "chartperiod:"+id+":168")}}
		}
		if strings.HasPrefix(q.Data, "chartperiod:") {
			parts := strings.Split(q.Data, ":")
			if len(parts) == 3 {
				text = "Выберите показатель:"
				for _, metric := range []struct{ label, key string }{{"CPU", "cpu"}, {"RAM", "ram"}, {"Доступность", "availability"}} {
					rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData(metric.label, "chart:"+parts[1]+":"+parts[2]+":"+metric.key)})
				}
			}
		}
		if strings.HasPrefix(q.Data, "chart:") {
			parts := strings.Split(q.Data, ":")
			if len(parts) == 4 {
				b.sendChart(ctx, q.Message.Chat.ID, parts[1], parts[2], parts[3])
				text = "График отправлен."
			}
		}
		if strings.HasPrefix(q.Data, "disable:") {
			if !b.IsAdmin(q.From.ID) {
				text = "Недостаточно прав."
			} else {
				n, e := strconv.ParseInt(strings.TrimPrefix(q.Data, "disable:"), 10, 64)
				if e != nil {
					text = "Некорректный ID"
				} else {
					rows = append(rows, []tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardButtonData("Подтвердить отключение", "confirm-disable:"+strconv.FormatInt(n, 10)), tgbotapi.NewInlineKeyboardButtonData("Отмена", "users")})
					text = fmt.Sprintf("Отключить доступ пользователя %d?", n)
				}
			}
		}
		if strings.HasPrefix(q.Data, "confirm-disable:") {
			if !b.IsAdmin(q.From.ID) {
				text = "Недостаточно прав."
			} else {
				n, e := strconv.ParseInt(strings.TrimPrefix(q.Data, "confirm-disable:"), 10, 64)
				if e != nil || n == 0 {
					text = "Некорректный ID"
				} else if e = b.store.DisableUser(ctx, n); e != nil {
					text = "Не удалось изменить доступ"
				} else {
					text = "Доступ отключён."
				}
			}
		}
	}
	msg := tgbotapi.NewMessage(q.Message.Chat.ID, text)
	if len(rows) > 0 {
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	}
	_, _ = api.Send(msg)
	_, _ = api.Request(tgbotapi.NewCallback(q.ID, ""))
}
func (b *Bot) send(chat int64, text string) {
	if api := b.apiClient(); api != nil {
		_, _ = api.Send(tgbotapi.NewMessage(chat, text))
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
		_, _ = api.Send(tgbotapi.NewMessage(id, text))
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
	lines = append(lines, "Сводка за сутки · "+time.Now().In(loc).Format("02.01.2006"))
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
		lines = append(lines, fmt.Sprintf("%s: %s, интернет %s, CPU %.1f%%, RAM %.1f%%, событий %d", server.Name, status, internet, cpu, ramPercent, incidents))
	}
	text := strings.Join(lines, "\n")
	ids, err := b.store.UserIDs(ctx)
	if err != nil {
		b.Notify(text)
		return
	}
	for _, id := range ids {
		if _, err := api.Send(tgbotapi.NewMessage(id, text)); err != nil {
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
		return "Нет данных"
	}
	return fmt.Sprintf("%s\nСервер: %s\nPing: %s\nИнтернет: %s\nАгент: %s\nCPU: %.1f%%\nRAM: %.1f / %.1f GiB\nДиск: %.1f / %.1f GiB\nАптайм: %s\nПроверка: %s\nОшибка: %s", name, status, ping, internet, agent, cpu, ru/1073741824, rt/1073741824, du/1073741824, dt/1073741824, time.Duration(uptime)*time.Second, at.Local().Format("02.01 15:04:05"), errText)
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
		b.send(chat, "Некорректные параметры графика.")
		return
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	samples, err := b.store.Samples(ctx, serverID, since, 300)
	if err != nil {
		b.send(chat, "Не удалось прочитать историю измерений.")
		return
	}
	pngData, err := charts.Render(metric, samples)
	if err != nil {
		b.send(chat, "За выбранный период пока нет данных.")
		return
	}
	labels := map[string]string{"cpu": "CPU, %", "ram": "RAM, %", "availability": "Доступность, %"}
	caption := fmt.Sprintf("%s · %s · %s\n%s — %s", name, labels[metric], hoursText+" ч", since.Local().Format("02.01 15:04"), time.Now().Local().Format("02.01 15:04"))
	photo := tgbotapi.NewPhoto(chat, tgbotapi.FileBytes{Name: "chart.png", Bytes: pngData})
	photo.Caption = caption
	if _, err = api.Send(photo); err != nil {
		b.send(chat, "Не удалось отправить график.")
	}
}
func (b *Bot) events() string {
	rows, e := b.store.DB.Query(`SELECT occurred_at,server_id,kind,message FROM events ORDER BY id DESC LIMIT 10`)
	if e != nil {
		return "Не удалось прочитать журнал"
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t time.Time
		var id, k, m string
		if rows.Scan(&t, &id, &k, &m) == nil {
			out = append(out, fmt.Sprintf("%s %s %s: %s", t.Local().Format("02.01 15:04"), id, k, m))
		}
	}
	if len(out) == 0 {
		return "Событий пока нет"
	}
	return strings.Join(out, "\n")
}
func (b *Bot) summary() string {
	var total, down int
	_ = b.store.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(status='DOWN'),0) FROM (SELECT server_id,status,MAX(checked_at) FROM samples GROUP BY server_id)`).Scan(&total, &down)
	return fmt.Sprintf("Последнее состояние: серверов %d, недоступны %d", total, down)
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
	return fmt.Sprintf("%s · %s · приоритет %d", server.Name, status, server.Priority)
}
