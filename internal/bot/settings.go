package bot

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"financer/internal/money"
	"financer/internal/storage"
	"financer/internal/tg"
)

const (
	maxCategory   = 30
	maxCategories = 40 // Telegram won't show more buttons in a single keyboard
)

func (b *Bot) showSettings(r *req) {
	var base string
	b.store.View(r.uid, func(u *storage.User) { base = u.BaseCurrency })
	b.show(r, fmt.Sprintf("⚙️ <b>Настройки</b>\n\nОсновная валюта: <b>%s</b>\nВ ней считаются общий итог и отчёты.", currencyLabel(base)), kb(
		row(btn("💱 Основная валюта", "s:b")),
		row(btn("🏷 Категории расходов", "s:c:e")),
		row(btn("🏷 Категории доходов", "s:c:i")),
		row(btn("📤 Экспорт в CSV", "s:x")),
	))
}

func catType(code string) storage.TxType {
	if code == "i" {
		return storage.TxIncome
	}
	return storage.TxExpense
}

func catCode(t storage.TxType) string {
	if t == storage.TxIncome {
		return "i"
	}
	return "e"
}

func (b *Bot) cbSettings(r *req, s *session, p []string) {
	switch arg(p, 0) {
	case "m":
		s.cancelInput()
		b.showSettings(r)

	case "b":
		s.cancelInput()
		codes := slices.Clone(popularCurrencies)
		b.store.View(r.uid, func(u *storage.User) {
			for _, a := range u.ActiveAccounts() {
				if !slices.Contains(codes, a.Currency) {
					codes = append(codes, a.Currency)
				}
			}
		})
		k := kb(currencyButtons(codes, "s:bc")...)
		k.Add(btn("« Назад", "s:m"))
		b.show(r, "💱 Выберите основную валюту:", k)

	case "bc":
		code := arg(p, 1)
		if !money.ValidCode(code) {
			b.stale(r)
			return
		}
		if err := b.store.Update(r.uid, func(u *storage.User) error {
			u.BaseCurrency = code
			return nil
		}); err != nil {
			b.fail(r, err)
			return
		}
		r.toast = "Основная валюта: " + code
		b.showSettings(r)

	case "c":
		s.cancelInput()
		b.showCategories(r, catType(arg(p, 1)))

	case "cd":
		typ, idx := catType(arg(p, 1)), int(argInt(p, 2))
		var removed string
		err := b.store.Update(r.uid, func(u *storage.User) error {
			cats := u.Categories(typ)
			if idx < 0 || idx >= len(cats) || catHash(cats[idx]) != arg(p, 3) {
				return errStale
			}
			removed = cats[idx]
			u.SetCategories(typ, slices.Delete(slices.Clone(cats), idx, idx+1))
			return nil
		})
		if err != nil {
			b.fail(r, err)
			return
		}
		r.toast = "Удалено: " + removed
		b.showCategories(r, typ)

	case "ca":
		typ := catType(arg(p, 1))
		s.reset()
		s.step, s.catType = stepCatAdd, typ
		b.show(r, "✍️ Напишите название новой категории. Можно с эмодзи, например <code>🐶 Питомец</code>",
			kb(row(btn("« Назад", "s:c:"+catCode(typ)))))

	case "x":
		s.cancelInput()
		b.exportCSV(r)
	}
}

func (b *Bot) showCategories(r *req, typ storage.TxType) {
	var cats []string
	b.store.View(r.uid, func(u *storage.User) { cats = slices.Clone(u.Categories(typ)) })
	title := "🏷 <b>Категории расходов</b>"
	if typ == storage.TxIncome {
		title = "🏷 <b>Категории доходов</b>"
	}
	code := catCode(typ)
	btns := make([]tg.InlineButton, 0, len(cats))
	for i, c := range cats {
		btns = append(btns, btn("✖ "+c, "s:cd:"+code+":"+strconv.Itoa(i)+":"+catHash(c)))
	}
	k := kb(grid(btns, 2)...)
	k.Add(btn("➕ Добавить", "s:ca:"+code), btn("« Назад", "s:m"))
	b.show(r, title+"\n\nНажмите на категорию, чтобы удалить её. Операции с ней останутся в истории.", k)
}

func (b *Bot) inputCategory(r *req, s *session, text string) {
	typ := s.catType
	name, ok := cleanText(text, maxCategory)
	if !ok {
		b.send(r, fmt.Sprintf("Название должно быть от 1 до %d символов.", maxCategory), nil)
		return
	}
	err := b.store.Update(r.uid, func(u *storage.User) error {
		cats := u.Categories(typ)
		if slices.ContainsFunc(cats, func(c string) bool { return strings.EqualFold(c, name) }) {
			return userError("Такая категория уже есть.")
		}
		if len(cats) >= maxCategories {
			return userError(fmt.Sprintf("Можно завести не больше %d категорий.", maxCategories))
		}
		u.SetCategories(typ, append(slices.Clone(cats), name))
		return nil
	})
	s.reset()
	if err != nil {
		b.fail(r, err)
		return
	}
	b.showCategories(r, typ)
}

func (b *Bot) exportCSV(r *req) {
	var buf bytes.Buffer
	buf.WriteString("\uFEFF") // BOM so that Excel opens UTF-8 correctly
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	n := 0
	b.store.View(r.uid, func(u *storage.User) {
		_ = w.Write([]string{"Дата", "Тип", "Счёт", "Валюта", "Сумма", "Счёт зачисления", "Валюта зачисления", "Сумма зачисления", "Категория", "Комментарий"})
		name := func(id int64) (string, string) {
			if a := u.Account(id); a != nil {
				return a.Name, a.Currency
			}
			return "", ""
		}
		for _, t := range u.Transactions {
			acc, cur := name(t.AccountID)
			rec := []string{t.CreatedAt.In(b.cfg.Location).Format("2006-01-02 15:04"), "", safeCell(acc), cur, "", "", "", "", safeCell(t.Category), safeCell(t.Note)}
			switch t.Type {
			case storage.TxExpense:
				rec[1], rec[4] = "Расход", money.Plain(-t.Amount)
			case storage.TxIncome:
				rec[1], rec[4] = "Доход", money.Plain(t.Amount)
			case storage.TxAdjust:
				rec[1], rec[4] = "Корректировка", money.Plain(t.Amount)
			case storage.TxTransfer:
				toAcc, toCur := name(t.ToAccountID)
				rec[1], rec[4] = "Перевод", money.Plain(-t.Amount)
				rec[5], rec[6], rec[7] = safeCell(toAcc), toCur, money.Plain(t.ToAmount)
			}
			_ = w.Write(rec)
			n++
		}
	})
	w.Flush()
	if err := w.Error(); err != nil {
		slog.Error("экспорт CSV", "err", err)
		r.toast = "Не удалось сформировать файл"
		return
	}
	if n == 0 {
		b.show(r, "📤 Пока нечего выгружать — операций нет.", kb(row(btn("« Назад", "s:m"))))
		return
	}
	name := "financer-" + time.Now().In(b.cfg.Location).Format("2006-01-02") + ".csv"
	if err := b.api.SendDocument(r.ctx, r.chatID, name, buf.Bytes(), fmt.Sprintf("📤 Экспорт: %d операций", n)); err != nil {
		slog.Error("отправка CSV", "user", r.uid, "err", err)
		r.toast = "⚠️ Не удалось отправить файл, попробуйте позже"
		return
	}
	r.toast = "Файл отправлен"
}

// safeCell guards against CSV formula injection when opened in Excel.
func safeCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
