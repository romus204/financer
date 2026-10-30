package bot

import (
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

// draft — операция, которую пользователь сейчас заполняет.
type draft struct {
	typ         storage.TxType
	accountID   int64
	toAccountID int64
	amount      int64
	toAmount    int64
	estimate    int64 // сумма зачисления по курсу, показанная на кнопке
	note        string
	msgID       int // сообщение с кнопками этого черновика
}

const maxNote = 200

func (b *Bot) needAccount(r *req) {
	b.show(r, "Сначала добавьте хотя бы один счёт — карту, банковский счёт или наличные.",
		kb(row(btn("➕ Добавить счёт", "a:new"))))
}

// --- расход / доход ---

func (b *Bot) startEntry(r *req, typ storage.TxType) {
	s := b.sess(r.uid)
	s.reset()
	var acc storage.Account
	found := false
	b.store.View(r.uid, func(u *storage.User) {
		if a := u.DefaultAccount(); a != nil {
			acc, found = *a, true
		}
	})
	if !found {
		b.needAccount(r)
		return
	}
	s.draft = &draft{typ: typ, accountID: acc.ID}
	s.step = stepAmount
	title, example := "➖ <b>Расход</b>", "350 кофе"
	if typ == storage.TxIncome {
		title, example = "➕ <b>Доход</b>", "50000 зарплата"
	}
	b.send(r, fmt.Sprintf("%s · %s\n\nВведите сумму и, если нужно, комментарий:\n<code>%s</code>\n\n<i>Счёт можно будет сменить на следующем шаге.</i>",
		title, accLabel(&acc), example), kb(row(btn("❌ Отмена", "x"))))
}

// quickEntry — ввод вида «350 кофе» без нажатия кнопок.
func (b *Bot) quickEntry(r *req, s *session, text string) {
	in, err := money.ParseInput(text)
	if err != nil {
		b.sendMenu(r, "🤔 Не понял. Чтобы записать расход, отправьте сумму и комментарий: <code>350 кофе</code>\n"+
			"Доход — со знаком плюс: <code>+50000 зарплата</code>\n\nИли выберите действие в меню 👇")
		return
	}
	typ := storage.TxExpense
	if in.Sign > 0 {
		typ = storage.TxIncome
	}
	s.reset()
	s.draft = &draft{typ: typ}
	b.fillDraft(r, s, in)
}

func (b *Bot) inputAmount(r *req, s *session, text string) {
	if s.draft == nil {
		s.reset()
		b.quickEntry(r, s, text)
		return
	}
	in, err := money.ParseInput(text)
	if err != nil {
		b.send(r, "Не понял сумму. Пример: <code>1500</code> или <code>350 кофе</code>", kb(row(btn("❌ Отмена", "x"))))
		return
	}
	if s.draft.typ == storage.TxTransfer {
		if from, _, ok := b.transferCurrencies(r.uid, s.draft); ok && in.Currency != "" && in.Currency != from {
			b.send(r, fmt.Sprintf("Сумма списания вводится в валюте счёта-источника (%s). Сумму зачисления я спрошу следующим шагом.", currencyLabel(from)),
				kb(row(btn("❌ Отмена", "tf:x"))))
			return
		}
		s.draft.amount, s.draft.note = in.Amount, truncate(in.Note, maxNote)
		b.afterTransferAmount(r, s)
		return
	}
	b.fillDraft(r, s, in)
}

// fillDraft подбирает счёт по валюте и показывает выбор категории.
func (b *Bot) fillDraft(r *req, s *session, in money.Input) {
	d := s.draft
	var acc storage.Account
	found, hasAny := false, false
	b.store.View(r.uid, func(u *storage.User) {
		hasAny = len(u.ActiveAccounts()) > 0
		a := u.ActiveAccount(d.accountID)
		if a == nil {
			a = u.DefaultAccount()
		}
		if in.Currency != "" && (a == nil || a.Currency != in.Currency) {
			a = u.AccountByCurrency(in.Currency)
		}
		if a != nil {
			acc, found = *a, true
		}
	})
	if !found {
		s.reset()
		if !hasAny {
			b.needAccount(r)
			return
		}
		b.send(r, fmt.Sprintf("У вас нет счёта в %s. Добавьте его — и повторите ввод.", in.Currency),
			kb(row(btn("➕ Добавить счёт", "a:new"))))
		return
	}
	d.accountID, d.amount, d.note = acc.ID, in.Amount, truncate(in.Note, maxNote)
	s.step = stepIdle
	d.msgID = b.sendScreen(r, b.renderDraft(r.uid, d, ""))
}

func (b *Bot) renderDraft(uid int64, d *draft, footer string) screen {
	var text string
	var k *tg.InlineKeyboard
	b.store.View(uid, func(u *storage.User) {
		a := u.Account(d.accountID)
		if a == nil {
			text = "⚠️ " + string(errAccountGone)
			return
		}
		title := "➖ <b>Расход: "
		if d.typ == storage.TxIncome {
			title = "➕ <b>Доход: "
		}
		var sb strings.Builder
		sb.WriteString(title + money.Format(d.amount, a.Currency) + "</b>\n")
		fmt.Fprintf(&sb, "%s · остаток %s\n", accLabel(a), money.Format(u.Balance(a.ID), a.Currency))
		if d.note != "" {
			fmt.Fprintf(&sb, "📝 %s\n", esc(d.note))
		}
		if footer != "" {
			sb.WriteString("\n" + footer)
			text, k = sb.String(), kb(row(btn("« Назад", "d:b")))
			return
		}
		sb.WriteString("\nВыберите категорию 👇")
		text = sb.String()

		cats := u.Categories(d.typ)
		btns := make([]tg.InlineButton, 0, len(cats)+1)
		for i, c := range cats {
			btns = append(btns, btn(c, "d:c:"+strconv.Itoa(i)+":"+catHash(c)))
		}
		if len(cats) == 0 {
			btns = append(btns, btn("✅ Без категории", "d:c:-1"))
		}
		k = kb(grid(btns, 2)...)
		k.Add(btn("💳 Счёт", "d:a"), btn("📝 Комментарий", "d:n"), btn("❌", "d:x"))
	})
	return screen{text, k}
}

func (b *Bot) cbDraft(r *req, s *session, p []string) {
	d := s.draft
	if d == nil || d.msgID != r.msgID || (d.typ != storage.TxExpense && d.typ != storage.TxIncome) {
		b.stale(r)
		return
	}
	switch arg(p, 0) {
	case "c":
		b.saveEntry(r, s, int(argInt(p, 1)), arg(p, 2))
	case "a":
		s.cancelInput()
		var text string
		k := kb()
		b.store.View(r.uid, func(u *storage.User) {
			bal := u.Balances()
			for _, a := range u.ActiveAccounts() {
				label := accButtonLabel(a, bal[a.ID])
				if a.ID == d.accountID {
					label = "✓ " + label
				}
				k.Add(btn(label, "d:s:"+strconv.FormatInt(a.ID, 10)))
			}
		})
		text = "Выберите счёт:"
		k.Add(btn("« Назад", "d:b"))
		d.msgID = b.show(r, text, k)
	case "s":
		id := argInt(p, 1)
		ok := false
		b.store.View(r.uid, func(u *storage.User) { ok = u.ActiveAccount(id) != nil })
		if ok {
			d.accountID = id
		}
		d.msgID = b.showScreen(r, b.renderDraft(r.uid, d, ""))
	case "n":
		s.step = stepNote
		d.msgID = b.showScreen(r, b.renderDraft(r.uid, d, "✍️ Отправьте комментарий сообщением."))
	case "b":
		s.cancelInput()
		d.msgID = b.showScreen(r, b.renderDraft(r.uid, d, ""))
	case "x":
		s.reset()
		b.show(r, "❌ Операция отменена.", nil)
	}
}

func (b *Bot) inputNote(r *req, s *session, text string) {
	d := s.draft
	if d == nil {
		s.reset()
		return
	}
	d.note = truncate(strings.Join(strings.Fields(text), " "), maxNote)
	s.cancelInput()
	// Пересылаем черновик вниз, чтобы он оказался под сообщением пользователя.
	if id := b.sendScreen(r, b.renderDraft(r.uid, d, "")); id != 0 {
		_ = b.api.DeleteMessage(r.ctx, r.chatID, d.msgID)
		d.msgID = id
	}
}

func (b *Bot) saveEntry(r *req, s *session, catIdx int, hash string) {
	d := s.draft
	var text string
	var txID int64
	err := b.store.Update(r.uid, func(u *storage.User) error {
		a := u.ActiveAccount(d.accountID)
		if a == nil {
			return errAccountGone
		}
		cat := ""
		if cats := u.Categories(d.typ); catIdx >= 0 {
			if catIdx >= len(cats) || catHash(cats[catIdx]) != hash {
				return errStale
			}
			cat = cats[catIdx]
		}
		t := u.AddTransaction(storage.Transaction{
			Type: d.typ, AccountID: a.ID, Amount: d.amount, Category: cat, Note: d.note,
		}, time.Now())
		txID = t.ID
		text = "✅ " + describeTx(u, t) + "\n\nОстаток — " + balancesOf(u, a.ID)
		return nil
	})
	if err != nil {
		var ue userError
		if !errors.As(err, &ue) {
			slog.Error("сохранение операции", "user", r.uid, "err", err)
			d.msgID = b.showScreen(r, b.renderDraft(r.uid, d, "⚠️ Не удалось сохранить. Попробуйте ещё раз."))
			return
		}
		b.fail(r, err)
		return
	}
	s.reset()
	r.toast = "Записано"
	b.show(r, text, undoKB(txID))
}

func undoKB(txID int64) *tg.InlineKeyboard {
	return kb(row(btn("↩️ Отменить", "u:"+strconv.FormatInt(txID, 10))))
}

func (b *Bot) undo(r *req, txID int64) {
	var accIDs []int64
	var balances string
	err := b.store.Update(r.uid, func(u *storage.User) error {
		t := u.Transaction(txID)
		if t == nil {
			return userError("Операция уже удалена.")
		}
		accIDs = []int64{t.AccountID}
		if t.ToAccountID != 0 {
			accIDs = append(accIDs, t.ToAccountID)
		}
		u.DeleteTransaction(txID)
		balances = balancesOf(u, accIDs...)
		return nil
	})
	if err != nil {
		b.fail(r, err)
		return
	}
	r.toast = "Отменено"
	text := "↩️ Операция отменена."
	if balances != "" {
		text += "\n\nОстаток — " + balances
	}
	b.show(r, text, nil)
}

// --- перевод ---

func (b *Bot) startTransfer(r *req) {
	s := b.sess(r.uid)
	s.reset()
	n := 0
	b.store.View(r.uid, func(u *storage.User) { n = len(u.ActiveAccounts()) })
	if n < 2 {
		b.send(r, "🔁 Для перевода нужно минимум два счёта — например, карта и наличные.",
			kb(row(btn("➕ Добавить счёт", "a:new"))))
		return
	}
	s.draft = &draft{typ: storage.TxTransfer}
	s.draft.msgID = b.sendScreen(r, b.renderTransferPick(r.uid, s.draft))
}

// renderTransferPick показывает выбор счёта «откуда» или «куда».
func (b *Bot) renderTransferPick(uid int64, d *draft) screen {
	k := kb()
	var text string
	b.store.View(uid, func(u *storage.User) {
		bal := u.Balances()
		if d.accountID == 0 {
			text = "🔁 <b>Перевод</b>\n\nОткуда?"
		} else {
			from, _ := accRef(u, d.accountID)
			text = "🔁 <b>Перевод</b>\n\nОткуда: " + from + "\nКуда?"
		}
		for _, a := range u.ActiveAccounts() {
			if a.ID == d.accountID {
				continue
			}
			action := "tf:f:"
			if d.accountID != 0 {
				action = "tf:t:"
			}
			k.Add(btn(accButtonLabel(a, bal[a.ID]), action+strconv.FormatInt(a.ID, 10)))
		}
	})
	if d.accountID != 0 {
		k.Add(btn("« Назад", "tf:back"), btn("❌ Отмена", "tf:x"))
	} else {
		k.Add(btn("❌ Отмена", "tf:x"))
	}
	return screen{text, k}
}

func (b *Bot) cbTransfer(r *req, s *session, p []string) {
	d := s.draft
	if d == nil || d.typ != storage.TxTransfer || d.msgID != r.msgID {
		b.stale(r)
		return
	}
	switch arg(p, 0) {
	case "f", "t":
		id := argInt(p, 1)
		var cur, label string
		b.store.View(r.uid, func(u *storage.User) {
			if a := u.ActiveAccount(id); a != nil {
				cur = a.Currency
			}
		})
		if cur == "" {
			b.fail(r, errAccountGone)
			return
		}
		if arg(p, 0) == "f" {
			d.accountID = id
			d.msgID = b.showScreen(r, b.renderTransferPick(r.uid, d))
			return
		}
		if id == d.accountID {
			b.stale(r)
			return
		}
		d.toAccountID = id
		s.step = stepAmount
		b.store.View(r.uid, func(u *storage.User) {
			from, fromCur := accRef(u, d.accountID)
			to, _ := accRef(u, id)
			label = fmt.Sprintf("🔁 <b>Перевод</b>\n%s → %s\n\nВведите сумму списания в %s (можно с комментарием):\n<code>5000</code>",
				from, to, currencyLabel(fromCur))
		})
		d.msgID = b.show(r, label, kb(row(btn("« Назад", "tf:back"), btn("❌ Отмена", "tf:x"))))
	case "back":
		s.cancelInput()
		d.accountID, d.toAccountID = 0, 0
		d.msgID = b.showScreen(r, b.renderTransferPick(r.uid, d))
	case "r":
		if s.step != stepTransferTo {
			b.stale(r)
			return
		}
		if d.estimate <= 0 {
			b.stale(r)
			return
		}
		d.toAmount = d.estimate
		b.saveTransfer(r, s)
	case "x":
		s.reset()
		b.show(r, "❌ Перевод отменён.", nil)
	}
}

func (b *Bot) transferCurrencies(uid int64, d *draft) (from, to string, ok bool) {
	b.store.View(uid, func(u *storage.User) {
		f, t := u.ActiveAccount(d.accountID), u.ActiveAccount(d.toAccountID)
		if f != nil && t != nil {
			from, to, ok = f.Currency, t.Currency, true
		}
	})
	return from, to, ok
}

func (b *Bot) afterTransferAmount(r *req, s *session) {
	d := s.draft
	from, to, ok := b.transferCurrencies(r.uid, d)
	if !ok {
		s.reset()
		b.fail(r, errAccountGone)
		return
	}
	if from == to {
		d.toAmount = d.amount
		b.saveTransfer(r, s)
		return
	}
	s.step = stepTransferTo
	var toLabel string
	b.store.View(r.uid, func(u *storage.User) { toLabel, _ = accRef(u, d.toAccountID) })
	text := fmt.Sprintf("🔁 Списано: <b>%s</b>\n\nСколько поступило на %s в %s?",
		money.Format(d.amount, from), toLabel, currencyLabel(to))
	k := kb()
	d.estimate = 0
	if est, ok := b.rates.Convert(r.ctx, d.amount, from, to); ok && est > 0 {
		d.estimate = est
		text += "\n<i>По текущему курсу ≈ " + money.Format(est, to) + "</i>"
		k.Add(btn("✅ "+money.Format(est, to)+" по курсу", "tf:r"))
	}
	k.Add(btn("❌ Отмена", "tf:x"))
	d.msgID = b.send(r, text, k)
}

func (b *Bot) inputTransferTo(r *req, s *session, text string) {
	d := s.draft
	if d == nil || d.typ != storage.TxTransfer {
		s.reset()
		return
	}
	v, err := money.Parse(text)
	if err != nil || v <= 0 {
		b.send(r, "Не понял сумму. Введите, сколько поступило, например <code>52,30</code>", kb(row(btn("❌ Отмена", "tf:x"))))
		return
	}
	d.toAmount = v
	b.saveTransfer(r, s)
}

func (b *Bot) saveTransfer(r *req, s *session) {
	d := s.draft
	var text string
	var txID int64
	err := b.store.Update(r.uid, func(u *storage.User) error {
		if u.ActiveAccount(d.accountID) == nil || u.ActiveAccount(d.toAccountID) == nil {
			return errAccountGone
		}
		t := u.AddTransaction(storage.Transaction{
			Type: storage.TxTransfer, AccountID: d.accountID, Amount: d.amount,
			ToAccountID: d.toAccountID, ToAmount: d.toAmount, Note: d.note,
		}, time.Now())
		txID = t.ID
		text = "✅ " + describeTx(u, t) + "\n\nОстатки:\n" + balancesOf(u, d.accountID, d.toAccountID)
		return nil
	})
	if err != nil {
		s.reset()
		b.fail(r, err)
		return
	}
	s.reset()
	r.toast = "Перевод записан"
	b.show(r, text, undoKB(txID))
}

// catHash — короткий отпечаток названия категории для callback-кнопок: защищает от
// срабатывания устаревшей кнопки, если список категорий успел измениться.
func catHash(name string) string {
	h := fnv.New32a()
	h.Write([]byte(name))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}
