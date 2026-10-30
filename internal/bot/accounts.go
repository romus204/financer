package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
)

type accountDraft struct {
	kind     storage.AccountKind
	currency string
	name     string
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
		sc = renderAccount(u, a)
	})
	if !found {
		b.show(r, "Счёт не найден.", kb(row(btn("« Все счета", "a:l"))))
		return
	}
	b.show(r, prefix+sc.text, sc.kb)
}

func renderAccount(u *storage.User, a *storage.Account) screen {
	isDefault := u.DefaultAccount() != nil && u.DefaultAccount().ID == a.ID
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s <b>%s</b>\n%s · %s\n\nБаланс: <b>%s</b>",
		kindIcon(a.Kind), esc(a.Name), kindName(a.Kind), currencyLabel(a.Currency),
		money.Format(u.Balance(a.ID), a.Currency))
	if isDefault {
		sb.WriteString("\n\n⭐ Основной счёт — на него записываются быстрые операции без указания валюты.")
	}
	id := strconv.FormatInt(a.ID, 10)
	k := kb(
		row(btn("🎯 Изменить баланс", "a:adj:"+id), btn("✏️ Переименовать", "a:rn:"+id)),
		row(btn("📜 История", "h:0:"+id)),
	)
	if !isDefault {
		k.Add(btn("⭐ Сделать основным", "a:def:"+id))
	}
	k.Add(btn("🗑 Удалить", "a:del:"+id), btn("« Все счета", "a:l"))
	return screen{sb.String(), k}
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
			row(btn("❌ Отмена", "a:l")),
		))

	case "k":
		kind := storage.AccountKind(arg(p, 1))
		if s.acc == nil || (kind != storage.KindBank && kind != storage.KindCash) {
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
		if s.acc == nil || s.acc.kind == "" {
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

	case "v":
		s.cancelInput()
		b.showAccount(r, id, "")

	case "adj", "rn":
		var name, cur string
		var bal int64
		b.store.View(r.uid, func(u *storage.User) {
			if a := u.ActiveAccount(id); a != nil {
				name, cur, bal = a.Name, a.Currency, u.Balance(a.ID)
			}
		})
		if name == "" {
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
		s.step = stepAccAdjust
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
	b.show(r, "Как назовём счёт? Например: <i>Тинькофф</i>, <i>Revolut</i>, <i>Кошелёк</i>, <i>Заначка</i>", k)
}

func (b *Bot) askAccountBalance(r *req, s *session) {
	s.step = stepAccBalance
	b.show(r, fmt.Sprintf("Сколько сейчас на «%s»? Введите сумму в %s.\n<i>Для кредитки с долгом — отрицательную, например <code>-15000</code>.</i>",
		esc(s.acc.name), currencyLabel(s.acc.currency)),
		kb(row(btn("0 — пустой", "a:b0"), btn("❌ Отмена", "a:l"))))
}

func (b *Bot) createAccount(r *req, s *session, balance int64) {
	ad := s.acc
	var id int64
	first := false
	err := b.store.Update(r.uid, func(u *storage.User) error {
		first = len(u.ActiveAccounts()) == 0
		now := time.Now()
		a := u.AddAccount(ad.name, ad.kind, ad.currency, now)
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
	case stepAccCurrency, stepAccName, stepAccBalance:
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
			delta = v - u.Balance(id)
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
