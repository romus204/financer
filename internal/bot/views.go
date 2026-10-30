package bot

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

// --- баланс ---

type accView struct {
	storage.Account
	Balance int64
}

func (b *Bot) showBalance(r *req) {
	var accs []accView
	var base string
	b.store.View(r.uid, func(u *storage.User) {
		bal := u.Balances()
		for _, a := range u.ActiveAccounts() {
			accs = append(accs, accView{Account: *a, Balance: bal[a.ID]})
		}
		base = u.BaseCurrency
	})
	if len(accs) == 0 {
		b.show(r, "💼 <b>Баланс</b>\n\nПока нет ни одного счёта.", kb(row(btn("➕ Добавить счёт", "a:new"))))
		return
	}

	var sb strings.Builder
	sb.WriteString("💼 <b>Баланс</b>\n")
	for _, kind := range []storage.AccountKind{storage.KindBank, storage.KindCash} {
		header := false
		for _, a := range accs {
			if a.Kind != kind {
				continue
			}
			if !header {
				title := "💳 <b>Карты и счета</b>"
				if kind == storage.KindCash {
					title = "💵 <b>Наличные</b>"
				}
				sb.WriteString("\n" + title + "\n")
				header = true
			}
			fmt.Fprintf(&sb, "%s — <b>%s</b>\n", esc(a.Name), money.Format(a.Balance, a.Currency))
		}
	}

	totals := map[string]int64{}
	for _, a := range accs {
		totals[a.Currency] += a.Balance
	}
	curs := make([]string, 0, len(totals))
	for c := range totals {
		curs = append(curs, c)
	}
	slices.SortFunc(curs, func(a, c string) int {
		switch {
		case a == base:
			return -1
		case c == base:
			return 1
		}
		return cmp.Compare(a, c)
	})

	if len(accs) > 1 {
		sb.WriteString("\n<b>По валютам</b>\n")
		for _, c := range curs {
			sb.WriteString(money.Format(totals[c], c) + "\n")
		}
	}

	if len(curs) > 1 || curs[0] != base {
		var sum int64
		var missing []string
		converted := false
		for _, c := range curs {
			if v, ok := b.rates.Convert(r.ctx, totals[c], c, base); ok {
				sum += v
				converted = true
			} else {
				missing = append(missing, money.Format(totals[c], c))
			}
		}
		if converted && (len(missing) < len(curs)) {
			fmt.Fprintf(&sb, "\n💰 Всего ≈ <b>%s</b>\n", money.Format(sum, base))
		}
		if len(missing) > 0 {
			if !converted || len(missing) == len(curs) {
				sb.WriteString("\n<i>Курсы валют сейчас недоступны — общий итог не посчитан.</i>")
			} else {
				sb.WriteString("<i>без учёта (нет курса): " + strings.Join(missing, ", ") + "</i>\n")
			}
		}
		if upd := b.rates.Updated(); !upd.IsZero() && converted {
			sb.WriteString("<i>Курс на " + upd.In(b.cfg.Location).Format("02.01 15:04") + "</i>")
		}
	}

	r.toast = "Обновлено"
	b.show(r, strings.TrimRight(sb.String(), "\n"), kb(row(btn("🔄 Обновить", "bal"), btn("🏦 Счета", "a:l"))))
}

// --- история ---

const historyPage = 10

func (b *Bot) showHistory(r *req, offset int, accID int64) {
	loc := b.cfg.Location
	now := time.Now().In(loc)
	var sb strings.Builder
	k := kb()
	b.store.View(r.uid, func(u *storage.User) {
		var list []*storage.Transaction
		for i := len(u.Transactions) - 1; i >= 0; i-- {
			t := u.Transactions[i]
			if accID == 0 || t.AccountID == accID || t.ToAccountID == accID {
				list = append(list, t)
			}
		}
		sb.WriteString("📜 <b>История</b>")
		if accID != 0 {
			label, _ := accRef(u, accID)
			sb.WriteString(" · " + label)
		}
		sb.WriteString("\n")
		if len(list) == 0 {
			sb.WriteString("\nОпераций пока нет.")
			return
		}
		if offset < 0 || offset >= len(list) {
			offset = (len(list) - 1) / historyPage * historyPage
		}
		end := min(offset+historyPage, len(list))

		lastDay := ""
		var nums []tg.InlineButton
		for i, t := range list[offset:end] {
			if day := dayTitle(t.CreatedAt.In(loc), now); day != lastDay {
				sb.WriteString("\n<b>" + day + "</b>\n")
				lastDay = day
			}
			n := offset + i + 1
			fmt.Fprintf(&sb, "%d. %s\n", n, txLine(u, t, loc))
			nums = append(nums, btn(strconv.Itoa(n), fmt.Sprintf("tx:%d:%d:%d", t.ID, offset, accID)))
		}
		sb.WriteString("\n<i>Нажмите номер, чтобы открыть или удалить операцию.</i>")
		for _, rw := range grid(nums, 5) {
			k.Add(rw...)
		}
		var nav []tg.InlineButton
		if end < len(list) {
			nav = append(nav, btn("« Раньше", fmt.Sprintf("h:%d:%d", end, accID)))
		}
		if offset > 0 {
			nav = append(nav, btn("Позже »", fmt.Sprintf("h:%d:%d", max(0, offset-historyPage), accID)))
		}
		k.Add(nav...)
	})
	if accID != 0 {
		k.Add(btn("« К счёту", "a:v:"+strconv.FormatInt(accID, 10)))
	}
	b.show(r, sb.String(), k)
}

func (b *Bot) showTx(r *req, id int64, offset int, accID int64) {
	var text string
	b.store.View(r.uid, func(u *storage.User) {
		if t := u.Transaction(id); t != nil {
			text = describeTx(u, t) + "\n🕒 " + t.CreatedAt.In(b.cfg.Location).Format("02.01.2006 15:04")
		}
	})
	back := btn("« К истории", fmt.Sprintf("h:%d:%d", offset, accID))
	if text == "" {
		b.show(r, "Операция не найдена.", kb(row(back)))
		return
	}
	b.show(r, text, kb(row(btn("🗑 Удалить", fmt.Sprintf("txd:%d:%d:%d", id, offset, accID)), back)))
}

func (b *Bot) deleteTx(r *req, id int64, offset int, accID int64) {
	err := b.store.Update(r.uid, func(u *storage.User) error {
		if !u.DeleteTransaction(id) {
			return userError("Операция уже удалена.")
		}
		return nil
	})
	if err != nil {
		b.fail(r, err)
		return
	}
	r.toast = "🗑 Операция удалена"
	b.showHistory(r, offset, accID)
}

// --- отчёт за месяц ---

type reportItem struct {
	typ      storage.TxType
	amount   int64
	currency string
	category string
}

func (b *Bot) showReport(r *req, month time.Time) {
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, b.cfg.Location)
	end := start.AddDate(0, 1, 0)

	var items []reportItem
	var base string
	b.store.View(r.uid, func(u *storage.User) {
		base = u.BaseCurrency
		for _, t := range u.Transactions {
			if (t.Type != storage.TxExpense && t.Type != storage.TxIncome) || t.CreatedAt.Before(start) || !t.CreatedAt.Before(end) {
				continue
			}
			_, cur := accRef(u, t.AccountID)
			items = append(items, reportItem{t.Type, t.Amount, cur, t.Category})
		}
	})

	totals := map[storage.TxType]int64{}
	byCat := map[storage.TxType]map[string]int64{storage.TxExpense: {}, storage.TxIncome: {}}
	unconverted := map[storage.TxType]map[string]int64{storage.TxExpense: {}, storage.TxIncome: {}}
	multiCurrency := false
	for _, it := range items {
		if it.currency != base {
			multiCurrency = true
		}
		v, ok := b.rates.Convert(r.ctx, it.amount, it.currency, base)
		if !ok {
			unconverted[it.typ][it.currency] += it.amount
			continue
		}
		totals[it.typ] += v
		byCat[it.typ][it.category] += v
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 <b>%s %d</b>\n", monthNames[start.Month()-1], start.Year())
	if len(items) == 0 {
		sb.WriteString("\nЗа этот месяц операций нет.")
	} else {
		fmt.Fprintf(&sb, "\n➕ Доходы: <b>%s</b>", money.Format(totals[storage.TxIncome], base))
		fmt.Fprintf(&sb, "\n➖ Расходы: <b>%s</b>", money.Format(totals[storage.TxExpense], base))
		fmt.Fprintf(&sb, "\n💰 Итог: <b>%s</b>\n", money.FormatSigned(totals[storage.TxIncome]-totals[storage.TxExpense], base))

		writeCategories(&sb, "Расходы по категориям", byCat[storage.TxExpense], totals[storage.TxExpense], base)
		writeCategories(&sb, "Доходы по категориям", byCat[storage.TxIncome], totals[storage.TxIncome], base)

		for _, typ := range []storage.TxType{storage.TxExpense, storage.TxIncome} {
			if len(unconverted[typ]) == 0 {
				continue
			}
			var parts []string
			for c, v := range unconverted[typ] {
				parts = append(parts, money.Format(v, c))
			}
			slices.Sort(parts)
			label := "расходы"
			if typ == storage.TxIncome {
				label = "доходы"
			}
			fmt.Fprintf(&sb, "\n<i>Не пересчитаны (нет курса) %s: %s</i>", label, strings.Join(parts, ", "))
		}
		if multiCurrency {
			fmt.Fprintf(&sb, "\n<i>Суммы в других валютах пересчитаны в %s по текущему курсу.</i>", base)
		}
	}

	prev, next := start.AddDate(0, -1, 0), end
	nav := []tg.InlineButton{btn("◀ "+monthNames[prev.Month()-1], "r:"+prev.Format("2006-01"))}
	if !next.After(b.currentMonth()) {
		nav = append(nav, btn(monthNames[next.Month()-1]+" ▶", "r:"+next.Format("2006-01")))
	}
	b.show(r, strings.TrimRight(sb.String(), "\n"), kb(nav))
}

func writeCategories(sb *strings.Builder, title string, byCat map[string]int64, total int64, base string) {
	if len(byCat) == 0 || total <= 0 {
		return
	}
	type kv struct {
		cat string
		sum int64
	}
	list := make([]kv, 0, len(byCat))
	for c, v := range byCat {
		list = append(list, kv{c, v})
	}
	slices.SortFunc(list, func(a, b kv) int { return cmp.Compare(b.sum, a.sum) })

	fmt.Fprintf(sb, "\n<b>%s</b>\n", title)
	for _, it := range list {
		share := float64(it.sum) / float64(total)
		fmt.Fprintf(sb, "%s\n%s %s · %d%%\n", catLabel(it.cat), bar(share), money.Format(it.sum, base), int(math.Round(share*100)))
	}
}

func bar(share float64) string {
	n := int(math.Round(share * 10))
	n = max(0, min(10, n))
	return strings.Repeat("▰", n) + strings.Repeat("▱", 10-n)
}
