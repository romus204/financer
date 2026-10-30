// Package bot implements the conversation logic of the finance-tracking Telegram bot.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"financer/internal/rates"
	"financer/internal/storage"
	"financer/internal/tg"
)

type Config struct {
	AllowedUsers map[int64]bool
	Location     *time.Location
}

type Bot struct {
	api      *tg.Client
	store    *storage.Store
	rates    *rates.Service
	cfg      Config
	sessions map[int64]*session
}

func New(api *tg.Client, store *storage.Store, rates *rates.Service, cfg Config) *Bot {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	return &Bot{api: api, store: store, rates: rates, cfg: cfg, sessions: map[int64]*session{}}
}

// step is the text input the bot is waiting for from the user.
type step int

const (
	stepIdle        step = iota
	stepAmount           // transaction amount
	stepTransferTo       // amount credited in a cross-currency transfer
	stepNote             // note for the draft
	stepAccCurrency      // currency code of a new account
	stepAccName          // name of a new account
	stepAccBalance       // opening balance of a new account
	stepAccRename
	stepAccAdjust
	stepCatAdd
)

type session struct {
	step    step
	draft   *draft
	acc     *accountDraft
	accID   int64
	catType storage.TxType
}

func (s *session) reset() { *s = session{} }

// cancelInput stops waiting for text input without touching the transaction draft.
func (s *session) cancelInput() { s.step = stepIdle }

// req is the context for handling a single update.
type req struct {
	ctx    context.Context
	uid    int64
	chatID int64
	msgID  int    // message that may be edited; 0 means send a new one
	toast  string // popup answer to a button press
}

// userError is an error whose text can be shown to the user.
type userError string

func (e userError) Error() string { return string(e) }

const (
	errAccountGone userError = "Счёт не найден — возможно, он был удалён."
	errStale       userError = "Это сообщение устарело, начните заново."
)

func (b *Bot) Run(ctx context.Context) error {
	if err := b.api.SetMyCommands(ctx, []tg.BotCommand{
		{Command: "start", Description: "Главное меню"},
		{Command: "balance", Description: "Баланс по счетам"},
		{Command: "help", Description: "Как пользоваться"},
		{Command: "cancel", Description: "Отменить текущее действие"},
	}); err != nil {
		slog.Warn("setMyCommands", "err", err)
	}

	var offset int64
	for {
		updates, err := b.api.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			delay := 3 * time.Second
			var apiErr *tg.APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
				delay = time.Duration(apiErr.RetryAfter) * time.Second
			}
			slog.Error("getUpdates", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			b.handleUpdate(ctx, u)
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, u tg.Update) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic при обработке обновления", "err", r, "stack", string(debug.Stack()))
		}
	}()
	switch {
	case u.Message != nil:
		b.onMessage(ctx, u.Message)
	case u.CallbackQuery != nil:
		b.onCallback(ctx, u.CallbackQuery)
	}
}

func (b *Bot) allowed(uid int64) bool { return b.cfg.AllowedUsers[uid] }

func (b *Bot) sess(uid int64) *session {
	s, ok := b.sessions[uid]
	if !ok {
		s = &session{}
		b.sessions[uid] = s
	}
	return s
}

func (b *Bot) onMessage(ctx context.Context, m *tg.Message) {
	// Finances are personal: only private chats are served.
	if m.From == nil || m.Chat.Type != "private" {
		return
	}
	r := &req{ctx: ctx, uid: m.From.ID, chatID: m.Chat.ID}
	if !b.allowed(r.uid) {
		b.send(r, "⛔ Доступ закрыт.\n\nВаш Telegram ID: <code>"+strconv.FormatInt(r.uid, 10)+
			"</code>\nДобавьте его в <code>ALLOWED_USER_IDS</code>, чтобы пользоваться ботом.", nil)
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}
	s := b.sess(r.uid)

	cmd := text
	if strings.HasPrefix(cmd, "/") {
		cmd, _, _ = strings.Cut(cmd, " ")
		cmd, _, _ = strings.Cut(cmd, "@")
	}
	switch cmd {
	case "/start":
		s.reset()
		b.cmdStart(r)
		return
	case "/help":
		s.cancelInput()
		b.cmdHelp(r)
		return
	case "/cancel":
		s.reset()
		b.sendMenu(r, "Отменено.")
		return
	case btnExpense:
		b.startEntry(r, storage.TxExpense)
		return
	case btnIncome:
		b.startEntry(r, storage.TxIncome)
		return
	case btnTransfer:
		b.startTransfer(r)
		return
	case btnBalance, "/balance":
		s.cancelInput()
		b.showBalance(r)
		return
	case btnHistory:
		s.cancelInput()
		b.showHistory(r, 0, 0)
		return
	case btnReport:
		s.cancelInput()
		b.showReport(r, b.currentMonth())
		return
	case btnAccounts:
		s.cancelInput()
		b.showAccounts(r)
		return
	case btnSettings:
		s.cancelInput()
		b.showSettings(r)
		return
	}

	if strings.HasPrefix(text, "/") {
		b.sendMenu(r, "Неизвестная команда. Доступно: /start, /balance, /help, /cancel")
		return
	}

	switch s.step {
	case stepAmount:
		b.inputAmount(r, s, text)
	case stepTransferTo:
		b.inputTransferTo(r, s, text)
	case stepNote:
		b.inputNote(r, s, text)
	case stepAccCurrency, stepAccName, stepAccBalance, stepAccRename, stepAccAdjust:
		b.inputAccount(r, s, text)
	case stepCatAdd:
		b.inputCategory(r, s, text)
	default:
		b.quickEntry(r, s, text)
	}
}

func (b *Bot) onCallback(ctx context.Context, q *tg.CallbackQuery) {
	r := &req{ctx: ctx, uid: q.From.ID}
	defer func() {
		if err := b.api.AnswerCallbackQuery(ctx, q.ID, r.toast); err != nil {
			slog.Warn("answerCallbackQuery", "err", err)
		}
	}()
	if q.Message == nil || q.Message.Chat.Type != "private" {
		return
	}
	if !b.allowed(r.uid) {
		r.toast = "⛔ Доступ закрыт"
		return
	}
	r.chatID, r.msgID = q.Message.Chat.ID, q.Message.MessageID
	s := b.sess(r.uid)

	p := strings.Split(q.Data, ":")
	switch p[0] {
	case "x":
		// A stale "Cancel" button must not discard a newer draft.
		if s.draft == nil || s.draft.msgID == 0 || s.draft.msgID == r.msgID {
			s.reset()
		}
		b.show(r, "Отменено.", nil)
	case "u":
		s.cancelInput()
		b.undo(r, argInt(p, 1))
	case "d":
		b.cbDraft(r, s, p[1:])
	case "tf":
		b.cbTransfer(r, s, p[1:])
	case "a":
		b.cbAccount(r, s, p[1:])
	case "bal":
		s.cancelInput()
		b.showBalance(r)
	case "h":
		s.cancelInput()
		b.showHistory(r, int(argInt(p, 1)), argInt(p, 2))
	case "tx":
		s.cancelInput()
		b.showTx(r, argInt(p, 1), int(argInt(p, 2)), argInt(p, 3))
	case "txd":
		s.cancelInput()
		b.deleteTx(r, argInt(p, 1), int(argInt(p, 2)), argInt(p, 3))
	case "r":
		s.cancelInput()
		month, err := time.ParseInLocation("2006-01", arg(p, 1), b.cfg.Location)
		if err != nil {
			month = b.currentMonth()
		}
		b.showReport(r, month)
	case "s":
		b.cbSettings(r, s, p[1:])
	}
}

func (b *Bot) cmdStart(r *req) {
	b.sendMenu(r, `👋 <b>Привет! Я веду учёт денег на картах, счетах и в наличных — в любых валютах.</b>

<b>Самый быстрый способ записать операцию</b> — просто отправь сообщение:
• <code>350 кофе</code> — расход с основного счёта
• <code>+50000 зарплата</code> — доход
• <code>20$ такси</code> или <code>20 usd такси</code> — расход со счёта в долларах

Остальное — в меню внизу 👇  Подробности: /help`)

	hasAccounts := false
	b.store.View(r.uid, func(u *storage.User) { hasAccounts = len(u.ActiveAccounts()) > 0 })
	if !hasAccounts {
		b.send(r, "Для начала добавь счёт — карту, банковский счёт или кошелёк с наличными.",
			kb(row(btn("➕ Добавить счёт", "a:new"))))
		return
	}
	b.showBalance(r)
}

func (b *Bot) cmdHelp(r *req) {
	b.sendMenu(r, `📖 <b>Как пользоваться</b>

<b>Быстрый ввод.</b> Отправь сумму и комментарий — бот предложит выбрать категорию:
• <code>350 кофе</code> — расход
• <code>+1500 кэшбэк</code> — доход
• <code>20$ такси</code>, <code>15 eur обед</code>, <code>5000 тг</code> — счёт подберётся по валюте
Суммы можно писать как <code>1 500,50</code> или <code>1500.5</code>.

<b>➖ Расход / ➕ Доход</b> — то же самое через кнопки. На экране выбора категории можно сменить счёт и добавить комментарий.

<b>🔁 Перевод</b> — между своими счетами: снятие наличных, пополнение карты, обмен валюты. Если валюты разные, бот спросит, сколько пришло (и подскажет по курсу).

<b>🏦 Счета</b> — добавить, переименовать, выбрать основной (⭐), удалить. <b>🎯 Изменить баланс</b> — если остаток в боте разошёлся с реальным, введи фактическую сумму, разница запишется корректировкой.

<b>💼 Баланс</b> — остатки по каждому счёту, итоги по валютам и общий итог в основной валюте.
<b>📜 История</b> — последние операции, любую можно открыть и удалить.
<b>📊 Отчёт</b> — доходы и расходы по категориям за месяц.
<b>⚙️ Настройки</b> — основная валюта, категории, экспорт в CSV.

После каждой записи есть кнопка <b>↩️ Отменить</b>. /cancel — прервать любое действие.`)
}

func arg(p []string, i int) string {
	if i < len(p) {
		return p[i]
	}
	return ""
}

func argInt(p []string, i int) int64 {
	n, _ := strconv.ParseInt(arg(p, i), 10, 64)
	return n
}

func (b *Bot) currentMonth() time.Time {
	now := time.Now().In(b.cfg.Location)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, b.cfg.Location)
}
