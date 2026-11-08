package bot

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

// --- balance ---

type accView struct {
	storage.Account
	Balance int64
	Paid    int64 // credit cards: money put on the card since the statement
}

func (b *Bot) showBalance(r *req) {
	var accs []accView
	var base string
	b.store.View(r.uid, func(u *storage.User) {
		bal := u.Balances()
		for _, a := range u.ActiveAccounts() {
			v := accView{Account: *a, Balance: bal[a.ID]}
			if a.MinPayment > 0 {
				v.Paid = u.PaidSince(a.ID, a.StatementAt)
			}
			accs = append(accs, v)
		}
		base = u.BaseCurrency
	})
	if len(accs) == 0 {
		b.show(r, "💼 <b>Баланс</b>\n\nПока нет ни одного счёта.", kb(row(btn("➕ Добавить счёт", "a:new"))))
		return
	}

	var sb strings.Builder
	sb.WriteString("💼 <b>Баланс</b>\n")
	sections := []struct {
		kind  storage.AccountKind
		title string
	}{
		{storage.KindBank, "💳 <b>Карты и счета</b>"},
		{storage.KindCash, "💵 <b>Наличные</b>"},
		{storage.KindCredit, "💸 <b>Кредитные карты</b>"},
		{storage.KindLoan, "🧾 <b>Кредиты</b>"},
	}
	for _, sec := range sections {
		header := false
		for _, a := range accs {
			if a.Kind != sec.kind {
				continue
			}
			if !header {
				sb.WriteString("\n" + sec.title + "\n")
				header = true
			}
			if !a.Kind.IsDebt() || a.Balance > 0 {
				fmt.Fprintf(&sb, "%s — <b>%s</b>\n", esc(a.Name), money.Format(a.Balance, a.Currency))
				continue
			}
			fmt.Fprintf(&sb, "%s — долг <b>%s</b>", esc(a.Name), money.Format(-a.Balance, a.Currency))
			if avail, ok := a.Available(a.Balance); ok {
				sb.WriteString(" · доступно " + money.Format(avail, a.Currency))
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString(upcomingPayments(accs, time.Now().In(b.cfg.Location)))

	// Own money and debts are totalled separately; debts are positive amounts owed.
	own, debts := map[string]int64{}, map[string]int64{}
	nOwn := 0
	for _, a := range accs {
		if a.Kind.IsDebt() {
			debts[a.Currency] -= a.Balance
		} else {
			own[a.Currency] += a.Balance
			nOwn++
		}
	}
	ownCurs, debtCurs := sortCurrencies(own, base), sortCurrencies(debts, base)
	hasDebts := len(debtCurs) > 0

	if nOwn > 1 {
		sb.WriteString("\n<b>По валютам</b>\n")
		for _, c := range ownCurs {
			sb.WriteString(money.Format(own[c], c) + "\n")
		}
	}
	if hasDebts {
		sb.WriteString("\n<b>Долги</b>\n")
		for _, c := range debtCurs {
			sb.WriteString(money.Format(debts[c], c) + "\n")
		}
	}

	foreign := slices.ContainsFunc(append(slices.Clone(ownCurs), debtCurs...), func(c string) bool { return c != base })
	if foreign || hasDebts {
		ownSum, ownMissing, ownConverted := b.sumIn(r.ctx, own, ownCurs, base)
		debtSum, debtMissing, debtConverted := b.sumIn(r.ctx, debts, debtCurs, base)
		for i, m := range debtMissing {
			debtMissing[i] = "долг " + m
		}
		missing := append(ownMissing, debtMissing...)
		converted := ownConverted || debtConverted
		approx := ""
		if foreign {
			approx = "≈ "
		}
		switch {
		case !converted:
			sb.WriteString("\n<i>Курсы валют сейчас недоступны — общий итог не посчитан.</i>")
		default:
			fmt.Fprintf(&sb, "\n💰 Всего %s<b>%s</b>\n", approx, money.Format(ownSum, base))
			if hasDebts {
				fmt.Fprintf(&sb, "💰 За вычетом долгов %s<b>%s</b>\n", approx, money.Format(ownSum-debtSum, base))
			}
			if len(missing) > 0 {
				sb.WriteString("<i>без учёта (нет курса): " + strings.Join(missing, ", ") + "</i>\n")
			}
		}
		if upd := b.rates.Updated(); !upd.IsZero() && converted && foreign {
			sb.WriteString("<i>Курс на " + upd.In(b.cfg.Location).Format("02.01 15:04") + "</i>")
		}
	}

	r.toast = "Обновлено"
	b.show(r, strings.TrimRight(sb.String(), "\n"), kb(row(btn("🔄 Обновить", "bal"), btn("🏦 Счета", "a:l"))))
}

// sortCurrencies returns the currencies of totals with the base currency first.
func sortCurrencies(totals map[string]int64, base string) []string {
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
	return curs
}

// sumIn converts per-currency totals into base; missing lists the amounts without a rate.
func (b *Bot) sumIn(ctx context.Context, totals map[string]int64, curs []string, base string) (sum int64, missing []string, converted bool) {
	for _, c := range curs {
		if v, ok := b.rates.Convert(ctx, totals[c], c, base); ok {
			sum += v
			converted = true
		} else {
			missing = append(missing, money.Format(totals[c], c))
		}
	}
	return sum, missing, converted
}

// upcomingPayments lists the next payments on credit cards and loans that have a debt:
// the monthly loan payment, the credit card minimum payment and the grace period end.
func upcomingPayments(accs []accView, now time.Time) string {
	type payment struct {
		day  string // YYYY-MM-DD, for sorting
		line string
	}
	today := civilDate(now)
	var list []payment
	add := func(date time.Time, a accView, what string) {
		prefix := ""
		if civilDate(date).Before(today) {
			prefix = "⚠️ "
		}
		list = append(list, payment{date.Format("2006-01-02"), prefix + paymentDate(date, now) + " — " + esc(a.Name) + what})
	}
	for _, a := range accs {
		if a.Balance >= 0 {
			continue
		}
		switch a.Kind {
		case storage.KindLoan:
			date, ok := a.NextPayment(now)
			if !ok {
				continue
			}
			what := ""
			if a.PaymentAmount > 0 {
				what = " · <b>" + money.Format(min(a.PaymentAmount, -a.Balance), a.Currency) + "</b>"
			}
			add(date, a, what)
		case storage.KindCredit:
			if left := minPaymentLeft(&a.Account, a.Balance, a.Paid); a.MinPayment > 0 && left > 0 && !a.MinPaymentDue.IsZero() {
				add(a.MinPaymentDue, a, " · мин. платёж <b>"+money.Format(left, a.Currency)+"</b>")
			}
			if !a.RepayBy.IsZero() {
				add(a.RepayBy, a, " · весь долг <b>"+money.Format(-a.Balance, a.Currency)+"</b>")
			}
		}
	}
	if len(list) == 0 {
		return ""
	}
	slices.SortStableFunc(list, func(a, b payment) int { return cmp.Compare(a.day, b.day) })
	var sb strings.Builder
	sb.WriteString("\n📅 <b>Ближайшие платежи</b>\n")
	for _, p := range list {
		sb.WriteString(p.line + "\n")
	}
	return sb.String()
}

// --- history ---

const historyPage = 10

func (b *Bot) showHistory(r *req, offset int, accID int64) {
	loc := b.cfg.Location
	now := time.Now().In(loc)
	var sb strings.Builder
	var base string
	var days []historyDay
	k := kb()
	b.store.View(r.uid, func(u *storage.User) {
		base = u.BaseCurrency
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

		// Counted over the whole list: a day can span several pages.
		totals := dayTotals(u, list, loc, now)
		lastDay := ""
		var nums []tg.InlineButton
		for i, t := range list[offset:end] {
			if day := dayTitle(t.CreatedAt.In(loc), now); day != lastDay {
				days = append(days, historyDay{title: day, total: totals[day]})
				lastDay = day
			}
			n := offset + i + 1
			d := &days[len(days)-1]
			d.lines = append(d.lines, fmt.Sprintf("%d. %s", n, txLine(u, t, loc)))
			nums = append(nums, btn(strconv.Itoa(n), fmt.Sprintf("tx:%d:%d:%d", t.ID, offset, accID)))
		}
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

	// Rendered outside View: a conversion may fetch the rates over the network.
	converted := false
	for _, d := range days {
		sb.WriteString("\n<b>" + d.title + "</b>\n")
		if line := d.total.line(base); line != "" {
			sb.WriteString(line + "\n")
		}
		if line, foreign := b.netLine(r.ctx, d.total, base); line != "" {
			sb.WriteString(line + "\n")
			converted = converted || foreign
		}
		sb.WriteString(strings.Join(d.lines, "\n") + "\n")
	}
	if len(days) > 0 {
		if converted {
			fmt.Fprintf(&sb, "\n<i>Суммы в других валютах пересчитаны в %s по текущему курсу.</i>", base)
		}
		sb.WriteString("\n<i>Нажмите номер, чтобы открыть или удалить операцию.</i>")
	}
	if accID != 0 {
		k.Add(btn("« К счёту", "a:v:"+strconv.FormatInt(accID, 10)))
	}
	b.show(r, sb.String(), k)
}

// historyDay is one day on a history page.
type historyDay struct {
	title string
	total dayTotal
	lines []string
}

// netLine formats income minus expenses in base; foreign reports whether another currency
// was converted. Empty if it would only repeat the single amount of the day or no rate is known.
func (b *Bot) netLine(ctx context.Context, d dayTotal, base string) (line string, foreign bool) {
	expCurs, incCurs := sortCurrencies(d.expense, base), sortCurrencies(d.income, base)
	curs := append(slices.Clone(expCurs), incCurs...)
	if len(curs) == 0 || (len(curs) == 1 && curs[0] == base) {
		return "", false
	}
	expense, expMissing, expOK := b.sumIn(ctx, d.expense, expCurs, base)
	income, incMissing, incOK := b.sumIn(ctx, d.income, incCurs, base)
	if !expOK && !incOK {
		return "", false
	}
	missing := append(expMissing, incMissing...)
	nonBase := len(curs)
	for _, c := range curs {
		if c == base {
			nonBase--
		}
	}
	foreign = nonBase > len(missing) // the base currency always converts
	approx := ""
	if foreign {
		approx = "≈ "
	}
	line = "<i>Итог в основной валюте: " + approx + money.FormatSigned(income-expense, base)
	if len(missing) > 0 {
		line += " (без учёта: " + strings.Join(missing, ", ") + ")"
	}
	return line + "</i>", foreign
}

// dayTotal holds a day's expenses and income per currency.
type dayTotal struct {
	expense, income map[string]int64
}

// dayTotals sums transactions by day title; transfers and adjustments are not counted.
func dayTotals(u *storage.User, list []*storage.Transaction, loc *time.Location, now time.Time) map[string]dayTotal {
	res := map[string]dayTotal{}
	for _, t := range list {
		if t.Type != storage.TxExpense && t.Type != storage.TxIncome {
			continue
		}
		day := dayTitle(t.CreatedAt.In(loc), now)
		d, ok := res[day]
		if !ok {
			d = dayTotal{map[string]int64{}, map[string]int64{}}
			res[day] = d
		}
		_, cur := accRef(u, t.AccountID)
		if t.Type == storage.TxExpense {
			d.expense[cur] += t.Amount
		} else {
			d.income[cur] += t.Amount
		}
	}
	return res
}

// line formats the total for the history view; empty if the day has no expenses or income.
func (d dayTotal) line(base string) string {
	format := func(sign string, totals map[string]int64) string {
		var parts []string
		for _, c := range sortCurrencies(totals, base) {
			parts = append(parts, money.Format(totals[c], c))
		}
		return sign + " " + strings.Join(parts, ", ")
	}
	var parts []string
	if len(d.expense) > 0 {
		parts = append(parts, format("➖", d.expense))
	}
	if len(d.income) > 0 {
		parts = append(parts, format("➕", d.income))
	}
	if len(parts) == 0 {
		return ""
	}
	return "<i>Итого: " + strings.Join(parts, " · ") + "</i>"
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

// --- monthly report ---

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
	losses := map[string]int64{} // exchange losses by the credited currency
	b.store.View(r.uid, func(u *storage.User) {
		base = u.BaseCurrency
		for _, t := range u.Transactions {
			if t.CreatedAt.Before(start) || !t.CreatedAt.Before(end) {
				continue
			}
			if loss, ok := t.ExchangeLoss(); ok {
				if _, cur := accRef(u, t.ToAccountID); cur != "" {
					losses[cur] += loss
				}
			}
			if t.Type != storage.TxExpense && t.Type != storage.TxIncome {
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
	if len(items) == 0 && len(losses) == 0 {
		sb.WriteString("\nЗа этот месяц операций нет.")
	} else {
		fmt.Fprintf(&sb, "\n➕ Доходы: <b>%s</b>", money.Format(totals[storage.TxIncome], base))
		fmt.Fprintf(&sb, "\n➖ Расходы: <b>%s</b>", money.Format(totals[storage.TxExpense], base))
		fmt.Fprintf(&sb, "\n💰 Итог: <b>%s</b>\n", money.FormatSigned(totals[storage.TxIncome]-totals[storage.TxExpense], base))

		if len(losses) > 0 {
			sb.WriteString("\n" + b.lossSummary(r.ctx, losses, base) + "\n")
			for c := range losses {
				multiCurrency = multiCurrency || c != base
			}
		}

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

// lossSummary totals exchange losses in base; amounts without a rate are listed separately.
func (b *Bot) lossSummary(ctx context.Context, losses map[string]int64, base string) string {
	var sum int64
	var converted bool
	var missing []string
	for _, c := range slices.Sorted(maps.Keys(losses)) {
		v := losses[c]
		if conv, ok := b.rates.Convert(ctx, v, c, base); ok {
			sum += conv
			converted = true
		} else {
			missing = append(missing, money.Format(v, c))
		}
	}
	var lines []string
	switch {
	case sum > 0:
		lines = append(lines, "💱 Потери на обмене: <b>"+money.Format(sum, base)+"</b>")
	case sum < 0:
		lines = append(lines, "💱 Выгода на обмене: <b>"+money.Format(-sum, base)+"</b>")
	case converted:
		lines = append(lines, "💱 Обмен по рыночному курсу")
	}
	if len(missing) > 0 {
		lines = append(lines, "<i>Потери на обмене не пересчитаны (нет курса): "+strings.Join(missing, ", ")+"</i>")
	}
	return strings.Join(lines, "\n")
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
