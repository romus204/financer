package bot

import (
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

const (
	btnExpense  = "➖ Расход"
	btnIncome   = "➕ Доход"
	btnTransfer = "🔁 Перевод"
	btnBalance  = "💼 Баланс"
	btnHistory  = "📜 История"
	btnReport   = "📊 Отчёт"
	btnAccounts = "🏦 Счета"
	btnSettings = "⚙️ Настройки"
)

var mainKeyboard = &tg.ReplyKeyboard{
	Keyboard: [][]tg.KeyboardButton{
		{{Text: btnExpense}, {Text: btnIncome}, {Text: btnTransfer}},
		{{Text: btnBalance}, {Text: btnHistory}, {Text: btnReport}},
		{{Text: btnAccounts}, {Text: btnSettings}},
	},
	ResizeKeyboard:        true,
	IsPersistent:          true,
	InputFieldPlaceholder: "350 кофе · +5000 зарплата",
}

var popularCurrencies = []string{"RUB", "USD", "EUR", "KZT", "GBP", "CNY", "TRY", "GEL", "AMD", "BYN", "AED", "THB"}

var monthNames = [...]string{"Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
	"Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь"}

var monthGenitive = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// --- отправка ---

func (b *Bot) send(r *req, text string, k *tg.InlineKeyboard) int {
	var markup any
	if k != nil {
		markup = k
	}
	m, err := b.api.SendMessage(r.ctx, r.chatID, text, markup)
	if err != nil {
		slog.Error("sendMessage", "err", err)
		return 0
	}
	return m.MessageID
}

// sendMenu отправляет сообщение с главным меню (reply-клавиатурой).
func (b *Bot) sendMenu(r *req, text string) {
	if _, err := b.api.SendMessage(r.ctx, r.chatID, text, mainKeyboard); err != nil {
		slog.Error("sendMessage", "err", err)
	}
}

// show редактирует текущее сообщение (если пришли из кнопки) или отправляет новое.
func (b *Bot) show(r *req, text string, k *tg.InlineKeyboard) int {
	if r.msgID != 0 {
		err := b.api.EditMessageText(r.ctx, r.chatID, r.msgID, text, k)
		if err == nil {
			return r.msgID
		}
		slog.Warn("editMessageText, отправляю новое сообщение", "err", err)
	}
	return b.send(r, text, k)
}

func (b *Bot) fail(r *req, err error) {
	var ue userError
	if errors.As(err, &ue) {
		b.show(r, "⚠️ "+string(ue), nil)
		return
	}
	slog.Error("ошибка обработки", "user", r.uid, "err", err)
	b.show(r, "⚠️ Не удалось сохранить данные. Попробуйте ещё раз.", nil)
}

func (b *Bot) stale(r *req) {
	r.toast = string(errStale)
	b.show(r, "⌛ "+string(errStale), nil)
}

// --- клавиатуры ---

func btn(text, data string) tg.InlineButton {
	return tg.InlineButton{Text: text, CallbackData: data}
}

func row(btns ...tg.InlineButton) []tg.InlineButton { return btns }

func kb(rows ...[]tg.InlineButton) *tg.InlineKeyboard {
	k := &tg.InlineKeyboard{InlineKeyboard: [][]tg.InlineButton{}}
	for _, r := range rows {
		k.Add(r...)
	}
	return k
}

func grid(btns []tg.InlineButton, perRow int) [][]tg.InlineButton {
	var rows [][]tg.InlineButton
	for len(btns) > 0 {
		n := min(perRow, len(btns))
		rows = append(rows, btns[:n])
		btns = btns[n:]
	}
	return rows
}

func currencyButtons(codes []string, prefix string) [][]tg.InlineButton {
	btns := make([]tg.InlineButton, 0, len(codes))
	for _, c := range codes {
		btns = append(btns, btn(currencyLabel(c), prefix+":"+c))
	}
	return grid(btns, 4)
}

func currencyLabel(code string) string {
	if sym := money.Symbol(code); sym != code {
		return sym + " " + code
	}
	return code
}

// --- форматирование ---

func esc(s string) string { return html.EscapeString(s) }

func kindIcon(k storage.AccountKind) string {
	if k == storage.KindCash {
		return "💵"
	}
	return "💳"
}

func kindName(k storage.AccountKind) string {
	if k == storage.KindCash {
		return "Наличные"
	}
	return "Карта / счёт"
}

func accLabel(a *storage.Account) string { return kindIcon(a.Kind) + " " + esc(a.Name) }

func accButtonLabel(a *storage.Account, balance int64) string {
	return kindIcon(a.Kind) + " " + a.Name + " · " + money.Format(balance, a.Currency)
}

// accRef — подпись счёта для операции (счёт мог быть архивирован).
func accRef(u *storage.User, id int64) (label, currency string) {
	if a := u.Account(id); a != nil {
		return accLabel(a), a.Currency
	}
	return "❓ удалённый счёт", "???"
}

// cleanText схлопывает пробелы и проверяет длину.
func cleanText(s string, maxRunes int) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	n := utf8.RuneCountInString(s)
	return s, n > 0 && n <= maxRunes
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func dayTitle(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "Сегодня"
	case now.AddDate(0, 0, -1).Format("2006-01-02") == t.Format("2006-01-02"):
		return "Вчера"
	case y1 == y2:
		return fmt.Sprintf("%d %s", d1, monthGenitive[m1-1])
	default:
		return fmt.Sprintf("%d %s %d", d1, monthGenitive[m1-1], y1)
	}
}

// describeTx — подробное описание операции.
func describeTx(u *storage.User, t *storage.Transaction) string {
	var sb strings.Builder
	from, cur := accRef(u, t.AccountID)
	switch t.Type {
	case storage.TxExpense:
		fmt.Fprintf(&sb, "➖ <b>Расход: %s</b>\n%s\n%s", money.Format(t.Amount, cur), catLabel(t.Category), from)
	case storage.TxIncome:
		fmt.Fprintf(&sb, "➕ <b>Доход: %s</b>\n%s\n%s", money.Format(t.Amount, cur), catLabel(t.Category), from)
	case storage.TxTransfer:
		to, toCur := accRef(u, t.ToAccountID)
		fmt.Fprintf(&sb, "🔁 <b>Перевод: %s</b>\n%s → %s", money.Format(t.Amount, cur), from, to)
		if toCur != cur {
			fmt.Fprintf(&sb, "\nЗачислено: <b>%s</b>", money.Format(t.ToAmount, toCur))
			if t.Amount > 0 && t.ToAmount > 0 {
				fmt.Fprintf(&sb, "\nКурс: %s", exchangeRate(t.Amount, cur, t.ToAmount, toCur))
			}
		}
	case storage.TxAdjust:
		fmt.Fprintf(&sb, "🎯 <b>Корректировка: %s</b>\n%s", money.FormatSigned(t.Amount, cur), from)
	}
	if t.Note != "" {
		fmt.Fprintf(&sb, "\n📝 %s", esc(t.Note))
	}
	return sb.String()
}

// exchangeRate показывает курс в «естественном» направлении: 1 USD = 95,5 ₽.
func exchangeRate(a int64, aCur string, b int64, bCur string) string {
	if a < b {
		a, aCur, b, bCur = b, bCur, a, aCur
	}
	x := float64(a) / float64(b)
	prec := 4
	if x >= 100 {
		prec = 2
	}
	s := strconv.FormatFloat(x, 'f', prec, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return fmt.Sprintf("1 %s = %s %s", money.Symbol(bCur), strings.Replace(s, ".", ",", 1), money.Symbol(aCur))
}

func catLabel(c string) string {
	if c == "" {
		return "🏷 Без категории"
	}
	return esc(c)
}

// txLine — однострочное описание для истории.
func txLine(u *storage.User, t *storage.Transaction, loc *time.Location) string {
	tm := t.CreatedAt.In(loc).Format("15:04")
	from, cur := accRef(u, t.AccountID)
	var s string
	switch t.Type {
	case storage.TxExpense:
		s = fmt.Sprintf("%s ➖ <b>%s</b> · %s · %s", tm, money.Format(t.Amount, cur), catLabel(t.Category), from)
	case storage.TxIncome:
		s = fmt.Sprintf("%s ➕ <b>%s</b> · %s · %s", tm, money.Format(t.Amount, cur), catLabel(t.Category), from)
	case storage.TxTransfer:
		to, toCur := accRef(u, t.ToAccountID)
		s = fmt.Sprintf("%s 🔁 <b>%s</b> %s → %s", tm, money.Format(t.Amount, cur), from, to)
		if toCur != cur {
			s += " (" + money.Format(t.ToAmount, toCur) + ")"
		}
	case storage.TxAdjust:
		s = fmt.Sprintf("%s 🎯 <b>%s</b> · %s", tm, money.FormatSigned(t.Amount, cur), from)
	}
	if t.Note != "" {
		s += " · <i>" + esc(truncate(t.Note, 40)) + "</i>"
	}
	return s
}

// balancesOf — строки «счёт: остаток» для затронутых операцией счетов.
func balancesOf(u *storage.User, ids ...int64) string {
	bal := u.Balances()
	var lines []string
	for _, id := range ids {
		if a := u.ActiveAccount(id); a != nil {
			lines = append(lines, fmt.Sprintf("%s: <b>%s</b>", accLabel(a), money.Format(bal[id], a.Currency)))
		}
	}
	return strings.Join(lines, "\n")
}

// screen — текст сообщения вместе с его inline-клавиатурой.
type screen struct {
	text string
	kb   *tg.InlineKeyboard
}

func (b *Bot) showScreen(r *req, sc screen) int { return b.show(r, sc.text, sc.kb) }
func (b *Bot) sendScreen(r *req, sc screen) int { return b.send(r, sc.text, sc.kb) }
