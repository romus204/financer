package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

type accountDraft struct {
	kind     storage.AccountKind
	currency string
	name     string
	// Credit cards and loans only.
	debt      int64
	limit     int64
	payAmount int64
	payDay    int
}

const (
	maxAccountName = 40
	maxAccounts    = 30
)

func (b *Bot) showAccounts(r *req) {
	k := kb()
	n := 0
	b.store.View(r.uid, func(u *storage.User) {
		bal := u.Balances()
		def := u.DefaultAccount()
		for _, a := range u.ActiveAccounts() {
			label := accButtonLabel(a, bal[a.ID])
			if def != nil && def.ID == a.ID {
				label = "⭐ " + label
			}
			k.Add(btn(label, "a:v:"+strconv.FormatInt(a.ID, 10)))
			n++
		}
	})
	k.Add(btn("➕ Добавить счёт", "a:new"))
	text := "🏦 <b>Счета</b>\n\nВыберите счёт, чтобы изменить баланс, переименовать или удалить его."
	if n == 0 {
		text = "🏦 <b>Счета</b>\n\nПока нет ни одного счёта."
	}
	b.show(r, text, k)
}

func (b *Bot) showAccount(r *req, id int64, prefix string) {
	var sc screen
	found := false
	b.store.View(r.uid, func(u *storage.User) {
		a := u.ActiveAccount(id)
		if a == nil {
			return
		}
		found = true
		sc = renderAccount(u, a, time.Now().In(b.cfg.Location))
	})
	if !found {
		b.show(r, "Счёт не найден.", kb(row(btn("« Все счета", "a:l"))))
		return
	}
	b.show(r, prefix+sc.text, sc.kb)
}

func renderAccount(u *storage.User, a *storage.Account, now time.Time) screen {
	isDefault := u.DefaultAccount() != nil && u.DefaultAccount().ID == a.ID
	bal := u.Balance(a.ID)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <b>%s</b>\n%s · %s\n\n", kindIcon(a.Kind), esc(a.Name), kindName(a.Kind), currencyLabel(a.Currency))
	if a.Kind.IsDebt() {
		sb.WriteString(debtLines(a, bal))
		if p := paymentLine(a, now); p != "" {
			sb.WriteString("\n" + p)
		}
	} else {
		fmt.Fprintf(&sb, "Баланс: <b>%s</b>", money.Format(bal, a.Currency))
	}
	if isDefault {
		sb.WriteString("\n\n⭐ Основной счёт — на него записываются быстрые операции без указания валюты.")
	}
	id := strconv.FormatInt(a.ID, 10)
	k := kb(row(btn("🎯 Изменить баланс", "a:adj:"+id), btn("✏️ Переименовать", "a:rn:"+id)))
	if a.Kind.IsDebt() {
		k = kb(row(btn("🎯 Изменить долг", "a:adj:"+id), btn("✏️ Переименовать", "a:rn:"+id)))
		debtRow := row(btn("📅 Платёж", "a:pay:"+id))
		if a.Kind == storage.KindCredit {
			debtRow = append(debtRow, btn("💳 Доступно", "a:lim:"+id))
		}
		k.Add(debtRow...)
	}
	k.Add(btn("📜 История", "h:0:"+id))
	if !isDefault {
		k.Add(btn("⭐ Сделать основным", "a:def:"+id))
	}
	k.Add(btn("🗑 Удалить", "a:del:"+id), btn("« Все счета", "a:l"))
	return screen{sb.String(), k}
}

// debtLines shows the debt (or overpayment) and, for a credit card, the available amount.
func debtLines(a *storage.Account, bal int64) string {
	text := fmt.Sprintf("Долг: <b>%s</b>", money.Format(max(0, -bal), a.Currency))
	if bal > 0 {
		text = fmt.Sprintf("Переплата: <b>%s</b>", money.Format(bal, a.Currency))
	}
	if avail, ok := a.Available(bal); ok {
		text += fmt.Sprintf("\nДоступно: <b>%s</b> из %s", money.Format(avail, a.Currency), money.Format(a.CreditLimit, a.Currency))
	}
	return text
}

func (b *Bot) cbAccount(r *req, s *session, p []string) {
	id := argInt(p, 1)
	switch arg(p, 0) {
	case "l":
		s.cancelInput()
		b.showAccounts(r)

	case "new":
		s.reset()
		n := 0
		b.store.View(r.uid, func(u *storage.User) { n = len(u.ActiveAccounts()) })
		if n >= maxAccounts {
			b.show(r, fmt.Sprintf("Можно завести не больше %d счетов. Удалите ненужные в 🏦 Счета.", maxAccounts), nil)
			return
		}
		s.acc = &accountDraft{}
		b.show(r, "🏦 <b>Новый счёт</b>\n\nЧто добавляем?", kb(
			row(btn("💳 Карта / банковский счёт", "a:k:bank")),
			row(btn("💵 Наличные", "a:k:cash")),
			row(btn("💸 Кредитная карта", "a:k:credit"), btn("🧾 Кредит", "a:k:loan")),
			row(btn("❌ Отмена", "a:l")),
		))

	case "k":
		kind := storage.AccountKind(arg(p, 1))
		if s.acc == nil || s.accID != 0 || (kind != storage.KindBank && kind != storage.KindCash && !kind.IsDebt()) {
			b.stale(r)
			return
		}
		s.acc.kind = kind
		k := kb(currencyButtons(popularCurrencies, "a:c")...)
		k.Add(btn("✍️ Другая валюта", "a:c:other"))
		k.Add(btn("❌ Отмена", "a:l"))
		b.show(r, kindIcon(kind)+" <b>Новый счёт</b>\n\nВ какой валюте?", k)

	case "c":
		code := arg(p, 1)
		if s.acc == nil || s.accID != 0 || s.acc.kind == "" {
			b.stale(r)
			return
		}
		if code == "other" {
			s.step = stepAccCurrency
			b.show(r, "Введите код валюты, например <code>CHF</code>, <code>JPY</code> или <code>USDT</code>:",
				kb(row(btn("❌ Отмена", "a:l"))))
			return
		}
		if !money.ValidCode(code) {
			b.stale(r)
			return
		}
		s.acc.currency = code
		b.askAccountName(r, s)

	case "nd":
		if s.acc == nil || s.acc.currency == "" || s.step != stepAccName {
			b.stale(r)
			return
		}
		s.acc.name = "Наличные " + money.Symbol(s.acc.currency)
		b.askAccountBalance(r, s)

	case "b0":
		if s.acc == nil || s.acc.name == "" || s.step != stepAccBalance {
			b.stale(r)
			return
		}
		b.createAccount(r, s, 0)

	case "d0":
		if s.acc == nil || s.accID != 0 || s.step != stepAccDebt {
			b.stale(r)
			return
		}
		s.acc.debt = 0
		b.afterDebt(r, s)

	case "ls":
		if s.acc == nil || s.accID != 0 || s.step != stepAccAvailable {
			b.stale(r)
			return
		}
		s.acc.limit = 0
		b.askPayAmount(r, s)

	case "ps":
		if s.acc == nil || s.step != stepAccPayAmount {
			b.stale(r)
			return
		}
		if s.accID == 0 {
			s.acc.payAmount = 0
		} // when editing, "skip" keeps the current value
		b.askPayDay(r, s)

	case "pds":
		if s.acc == nil || s.step != stepAccPayDay {
			b.stale(r)
			return
		}
		if s.accID == 0 {
			s.acc.payDay = 0
		}
		b.finishPayment(r, s)

	case "pay":
		var acc storage.Account
		b.store.View(r.uid, func(u *storage.User) {
			if a := u.ActiveAccount(id); a != nil {
				acc = *a
			}
		})
		if acc.ID == 0 || !acc.Kind.IsDebt() {
			b.fail(r, errAccountGone)
			return
		}
		s.reset()
		s.accID = id
		s.acc = &accountDraft{kind: acc.Kind, currency: acc.Currency, name: acc.Name,
			payAmount: acc.PaymentAmount, payDay: acc.PaymentDay}
		b.askPayAmount(r, s)

	case "pc":
		s.reset()
		err := b.store.Update(r.uid, func(u *storage.User) error {
			a := u.ActiveAccount(id)
			if a == nil || !a.Kind.IsDebt() {
				return errAccountGone
			}
			a.PaymentAmount, a.PaymentDay = 0, 0
			return nil
		})
		if err != nil {
			b.fail(r, err)
			return
		}
		r.toast = "Платёж удалён"
		b.showAccount(r, id, "")

	case "v":
		s.cancelInput()
		b.showAccount(r, id, "")

	case "adj", "rn", "lim":
		var name, cur string
		var kind storage.AccountKind
		var bal int64
		b.store.View(r.uid, func(u *storage.User) {
			if a := u.ActiveAccount(id); a != nil {
				name, cur, kind, bal = a.Name, a.Currency, a.Kind, u.Balance(a.ID)
			}
		})
		if name == "" || (arg(p, 0) == "lim" && kind != storage.KindCredit) {
			b.fail(r, errAccountGone)
			return
		}
		s.reset()
		s.accID = id
		back := kb(row(btn("« Назад", "a:v:"+strconv.FormatInt(id, 10))))
		if arg(p, 0) == "rn" {
			s.step = stepAccRename
			b.show(r, fmt.Sprintf("✏️ Новое название для «%s»:", esc(name)), back)
			return
		}
		if arg(p, 0) == "lim" {
			s.step = stepAccLimit
			b.show(r, fmt.Sprintf("💳 Сколько сейчас доступно по карте «%s»? Введите сумму в %s — лимит пересчитается с учётом долга.",
				esc(name), currencyLabel(cur)), back)
			return
		}
		s.step = stepAccAdjust
		if kind.IsDebt() {
			b.show(r, fmt.Sprintf("🎯 Какой сейчас долг по «%s»?\n\nВ боте: <b>%s</b>\nВведите фактический долг в %s — разница запишется корректировкой.",
				esc(name), money.Format(max(0, -bal), cur), currencyLabel(cur)), back)
			return
		}
		b.show(r, fmt.Sprintf("🎯 Сколько сейчас реально на «%s»?\n\nВ боте: <b>%s</b>\nВведите фактический баланс в %s — разница запишется корректировкой.",
			esc(name), money.Format(bal, cur), currencyLabel(cur)), back)

	case "def":
		s.cancelInput()
		err := b.store.Update(r.uid, func(u *storage.User) error {
			if u.ActiveAccount(id) == nil {
				return errAccountGone
			}
			u.DefaultAccountID = id
			return nil
		})
		if err != nil {
			b.fail(r, err)
			return
		}
		r.toast = "⭐ Теперь это основной счёт"
		b.showAccount(r, id, "")

	case "del":
		s.cancelInput()
		var text string
		b.store.View(r.uid, func(u *storage.User) {
			a := u.ActiveAccount(id)
			if a == nil {
				return
			}
			text = fmt.Sprintf("🗑 Удалить счёт «%s»?\n\nБаланс: <b>%s</b>", esc(a.Name), money.Format(u.Balance(id), a.Currency))
			if u.HasTransactions(id) {
				text += "\n\nСчёт исчезнет из списков и итогов, но операции по нему останутся в истории."
			}
		})
		if text == "" {
			b.fail(r, errAccountGone)
			return
		}
		sid := strconv.FormatInt(id, 10)
		b.show(r, text, kb(row(btn("🗑 Да, удалить", "a:dy:"+sid), btn("Нет", "a:v:"+sid))))

	case "dy":
		s.cancelInput()
		err := b.store.Update(r.uid, func(u *storage.User) error {
			if u.ActiveAccount(id) == nil {
				return errAccountGone
			}
			u.RemoveAccount(id)
			return nil
		})
		if err != nil {
			b.fail(r, err)
			return
		}
		r.toast = "Счёт удалён"
		b.showAccounts(r)
	}
}

func (b *Bot) askAccountName(r *req, s *session) {
	s.step = stepAccName
	k := kb()
	if s.acc.kind == storage.KindCash {
		k.Add(btn("Наличные "+money.Symbol(s.acc.currency), "a:nd"))
	}
	k.Add(btn("❌ Отмена", "a:l"))
	examples := "<i>Тинькофф</i>, <i>Revolut</i>, <i>Кошелёк</i>, <i>Заначка</i>"
	switch s.acc.kind {
	case storage.KindCredit:
		examples = "<i>Тинькофф Платинум</i>, <i>Альфа 100 дней</i>"
	case storage.KindLoan:
		examples = "<i>Ипотека</i>, <i>Автокредит</i>, <i>Рассрочка на ноутбук</i>"
	}
	b.show(r, "Как назовём счёт? Например: "+examples, k)
}

// askAccountBalance asks for the opening balance or, for a credit card or loan, the debt.
func (b *Bot) askAccountBalance(r *req, s *session) {
	if s.acc.kind.IsDebt() {
		s.step = stepAccDebt
		b.show(r, fmt.Sprintf("Какой сейчас долг по «%s»? Введите сумму в %s.", esc(s.acc.name), currencyLabel(s.acc.currency)),
			kb(row(btn("0 — долга нет", "a:d0"), btn("❌ Отмена", "a:l"))))
		return
	}
	s.step = stepAccBalance
	b.show(r, fmt.Sprintf("Сколько сейчас на «%s»? Введите сумму в %s.", esc(s.acc.name), currencyLabel(s.acc.currency)),
		kb(row(btn("0 — пустой", "a:b0"), btn("❌ Отмена", "a:l"))))
}

func (b *Bot) afterDebt(r *req, s *session) {
	if s.acc.kind != storage.KindCredit {
		b.askPayAmount(r, s)
		return
	}
	s.step = stepAccAvailable
	b.show(r, fmt.Sprintf("Сколько сейчас доступно по карте? Введите сумму в %s.\n<i>Лимит посчитаю как доступно + долг.</i>", currencyLabel(s.acc.currency)),
		kb(row(btn("Пропустить", "a:ls"), btn("❌ Отмена", "a:l"))))
}

// accDraftBack returns to the account being edited or cancels the new account.
func accDraftBack(s *session) tg.InlineButton {
	if s.accID != 0 {
		return btn("« Назад", "a:v:"+strconv.FormatInt(s.accID, 10))
	}
	return btn("❌ Отмена", "a:l")
}

func (b *Bot) askPayAmount(r *req, s *session) {
	s.step = stepAccPayAmount
	k := kb(row(btn("Пропустить", "a:ps"), accDraftBack(s)))
	if s.accID != 0 {
		k.Add(btn("🗑 Убрать платёж", "a:pc:"+strconv.FormatInt(s.accID, 10)))
	}
	b.show(r, fmt.Sprintf("📅 Сколько нужно платить каждый месяц по «%s»? Введите сумму в %s.\n<i>Для кредитки — минимальный платёж. Можно пропустить.</i>",
		esc(s.acc.name), currencyLabel(s.acc.currency)), k)
}

func (b *Bot) askPayDay(r *req, s *session) {
	s.step = stepAccPayDay
	b.show(r, "📅 Какого числа платёж? Введите день месяца, например <code>15</code>, или дату — <code>15.11</code>.\n<i>Можно пропустить.</i>",
		kb(row(btn("Пропустить", "a:pds"), accDraftBack(s))))
}

// finishPayment completes the payment questions: saves them to the edited account or
// creates the new one.
func (b *Bot) finishPayment(r *req, s *session) {
	if s.accID == 0 {
		b.createAccount(r, s, -s.acc.debt)
		return
	}
	id, ad := s.accID, s.acc
	err := b.store.Update(r.uid, func(u *storage.User) error {
		a := u.ActiveAccount(id)
		if a == nil {
			return errAccountGone
		}
		a.PaymentAmount, a.PaymentDay = ad.payAmount, ad.payDay
		return nil
	})
	s.reset()
	if err != nil {
		b.fail(r, err)
		return
	}
	b.showAccount(r, id, "✅ Платёж сохранён.\n\n")
}

// parsePayDay accepts a day of month («15») or a date («15.11», «15.11.2026»).
func parsePayDay(text string) (int, bool) {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "./-"); i > 0 {
		text = text[:i]
	}
	d, err := strconv.Atoi(text)
	return d, err == nil && d >= 1 && d <= 31
}

func (b *Bot) createAccount(r *req, s *session, balance int64) {
	ad := s.acc
	var id int64
	first := false
	err := b.store.Update(r.uid, func(u *storage.User) error {
		first = len(u.ActiveAccounts()) == 0
		now := time.Now()
		a := u.AddAccount(ad.name, ad.kind, ad.currency, now)
		a.CreditLimit, a.PaymentAmount, a.PaymentDay = ad.limit, ad.payAmount, ad.payDay
		if balance != 0 {
			u.AddTransaction(storage.Transaction{Type: storage.TxAdjust, AccountID: a.ID, Amount: balance, Note: "Начальный остаток"}, now)
		}
		id = a.ID
		return nil
	})
	if err != nil {
		b.fail(r, err)
		return
	}
	s.reset()
	b.showAccount(r, id, "✅ Счёт добавлен!\n\n")
	if first {
		b.sendMenu(r, "🎉 Готово! Теперь записывайте операции прямо сообщением:\n<code>350 кофе</code> — расход, <code>+50000 зарплата</code> — доход.\n\nДобавить ещё счета (наличные, другие валюты) можно в 🏦 Счета.")
	}
}

func (b *Bot) inputAccount(r *req, s *session, text string) {
	cancel := kb(row(btn("❌ Отмена", "a:l")))
	switch s.step {
	case stepAccCurrency, stepAccName, stepAccBalance, stepAccDebt, stepAccAvailable, stepAccPayAmount, stepAccPayDay:
		if s.acc == nil {
			s.reset()
			return
		}
	}

	switch s.step {
	case stepAccCurrency:
		code, ok := money.ParseCurrency(text)
		if !ok {
			code = strings.ToUpper(strings.TrimSpace(text))
			ok = money.ValidCode(code)
		}
		if !ok {
			b.send(r, "Не похоже на код валюты. Примеры: <code>CHF</code>, <code>JPY</code>, <code>USDT</code>", cancel)
			return
		}
		s.acc.currency = code
		b.askAccountName(r, s)

	case stepAccName:
		name, ok := cleanText(text, maxAccountName)
		if !ok {
			b.send(r, fmt.Sprintf("Название должно быть от 1 до %d символов.", maxAccountName), cancel)
			return
		}
		s.acc.name = name
		b.askAccountBalance(r, s)

	case stepAccBalance:
		v, err := money.Parse(text)
		if err != nil {
			b.send(r, "Не понял сумму. Пример: <code>12500</code> или <code>-3000,50</code>", cancel)
			return
		}
		b.createAccount(r, s, v)

	case stepAccDebt:
		v, err := money.Parse(text)
		if err != nil || v < 0 {
			b.send(r, "Не понял сумму. Введите долг положительным числом, например <code>15000</code>", cancel)
			return
		}
		s.acc.debt = v
		b.afterDebt(r, s)

	case stepAccAvailable:
		v, err := money.Parse(text)
		if err != nil || v < 0 {
			b.send(r, "Не понял сумму. Пример: <code>85000</code>", cancel)
			return
		}
		if v+s.acc.debt <= 0 {
			b.send(r, "Лимит получается нулевым. Введите доступную сумму или нажмите «Пропустить».",
				kb(row(btn("Пропустить", "a:ls"), btn("❌ Отмена", "a:l"))))
			return
		}
		s.acc.limit = v + s.acc.debt
		b.askPayAmount(r, s)

	case stepAccPayAmount:
		v, err := money.Parse(text)
		if err != nil || v <= 0 {
			b.send(r, "Не понял сумму. Пример: <code>5000</code>", kb(row(accDraftBack(s))))
			return
		}
		s.acc.payAmount = v
		b.askPayDay(r, s)

	case stepAccPayDay:
		d, ok := parsePayDay(text)
		if !ok {
			b.send(r, "Введите число от 1 до 31, например <code>15</code>", kb(row(accDraftBack(s))))
			return
		}
		s.acc.payDay = d
		b.finishPayment(r, s)

	case stepAccLimit:
		v, err := money.Parse(text)
		if err != nil || v < 0 {
			b.send(r, "Не понял сумму. Пример: <code>85000</code>", nil)
			return
		}
		id := s.accID
		err = b.store.Update(r.uid, func(u *storage.User) error {
			a := u.ActiveAccount(id)
			if a == nil {
				return errAccountGone
			}
			if a.Kind != storage.KindCredit {
				return errAccountGone
			}
			if v-u.Balance(id) <= 0 {
				return userError("Доступная сумма не может быть меньше переплаты по карте.")
			}
			a.CreditLimit = v - u.Balance(id)
			return nil
		})
		s.reset()
		if err != nil {
			b.fail(r, err)
			return
		}
		b.showAccount(r, id, "✅ Доступная сумма обновлена.\n\n")

	case stepAccRename:
		name, ok := cleanText(text, maxAccountName)
		if !ok {
			b.send(r, fmt.Sprintf("Название должно быть от 1 до %d символов.", maxAccountName), nil)
			return
		}
		id := s.accID
		err := b.store.Update(r.uid, func(u *storage.User) error {
			a := u.ActiveAccount(id)
			if a == nil {
				return errAccountGone
			}
			a.Name = name
			return nil
		})
		s.reset()
		if err != nil {
			b.fail(r, err)
			return
		}
		b.showAccount(r, id, "✅ Переименовано.\n\n")

	case stepAccAdjust:
		v, err := money.Parse(text)
		if err != nil {
			b.send(r, "Не понял сумму. Пример: <code>12500</code> или <code>-3000,50</code>", nil)
			return
		}
		id := s.accID
		var delta int64
		var cur string
		err = b.store.Update(r.uid, func(u *storage.User) error {
			a := u.ActiveAccount(id)
			if a == nil {
				return errAccountGone
			}
			cur = a.Currency
			target := v
			if a.Kind.IsDebt() {
				if v < 0 {
					return userError("Долг не может быть отрицательным.")
				}
				target = -v // the user enters the debt
			}
			delta = target - u.Balance(id)
			if delta != 0 {
				u.AddTransaction(storage.Transaction{Type: storage.TxAdjust, AccountID: id, Amount: delta, Note: "Корректировка баланса"}, time.Now())
			}
			return nil
		})
		s.reset()
		if err != nil {
			b.fail(r, err)
			return
		}
		prefix := "Баланс и так совпадает 👌\n\n"
		if delta != 0 {
			prefix = "✅ Баланс скорректирован: " + money.FormatSigned(delta, cur) + "\n\n"
		}
		b.showAccount(r, id, prefix)
	}
}
