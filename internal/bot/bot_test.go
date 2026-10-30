package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"financer/internal/money"
	"financer/internal/rates"
	"financer/internal/storage"
	"financer/internal/tg"
)

const (
	testToken = "TEST:TOKEN"
	testUser  = int64(100)
)

type call struct {
	Method string
	Body   map[string]any
}

func (c call) text() string { s, _ := c.Body["text"].(string); return s }

// buttons returns all callback_data values of the call's inline keyboard.
func (c call) buttons() []string {
	var res []string
	rm, _ := c.Body["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	for _, r := range rows {
		for _, b := range r.([]any) {
			if d, ok := b.(map[string]any)["callback_data"].(string); ok {
				res = append(res, d)
			}
		}
	}
	return res
}

func (c call) hasButton(data string) bool {
	for _, d := range c.buttons() {
		if d == data {
			return true
		}
	}
	return false
}

type fakeTG struct {
	mu     sync.Mutex
	calls  []call
	nextID int
	// failEdit makes editMessageText return an API error.
	failEdit bool
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + testToken + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "bad path "+r.URL.Path, 404)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	raw, _ := io.ReadAll(r.Body)
	body := map[string]any{}
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	f.calls = append(f.calls, call{method, body})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch method {
	case "sendMessage":
		f.mu.Lock()
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":%v,"type":"private"}}}`, id, body["chat_id"])
	case "editMessageText":
		if f.failEdit {
			fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	default:
		fmt.Fprint(w, `{"ok":true,"result":true}`)
	}
}

func (f *fakeTG) byMethod(m string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []call
	for _, c := range f.calls {
		if c.Method == m {
			res = append(res, c)
		}
	}
	return res
}

func (f *fakeTG) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

// last returns the most recent send/edit call (the visible bot reaction).
func (f *fakeTG) last(t *testing.T) call {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if m := f.calls[i].Method; m == "sendMessage" || m == "editMessageText" {
			return f.calls[i]
		}
	}
	t.Fatal("no messages recorded")
	return call{}
}

func (f *fakeTG) lastToast(t *testing.T) string {
	t.Helper()
	cs := f.byMethod("answerCallbackQuery")
	if len(cs) == 0 {
		t.Fatal("no answerCallbackQuery recorded")
	}
	s, _ := cs[len(cs)-1].Body["text"].(string)
	return s
}

// lastSentID is the message_id of the latest sendMessage.
func (f *fakeTG) lastSentID() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nextID
}

type harness struct {
	t     *testing.T
	b     *Bot
	tg    *fakeTG
	ctx   context.Context
	upd   int64
	cbSeq int
}

func newHarness(t *testing.T, ratesURL string) *harness {
	t.Helper()
	f := &fakeTG{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	store, err := storage.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	b := New(tg.NewWithURL(testToken, srv.URL+"/bot"), store, rates.New(ratesURL), Config{
		AllowedUsers: map[int64]bool{testUser: true},
		Location:     time.UTC,
	})
	return &harness{t: t, b: b, tg: f, ctx: context.Background()}
}

func ratesServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":"success","rates":{"USD":1,"RUB":90,"EUR":0.9}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *harness) say(uid int64, text string) {
	h.upd++
	h.b.handleUpdate(h.ctx, tg.Update{UpdateID: h.upd, Message: &tg.Message{
		MessageID: int(h.upd), From: &tg.User{ID: uid}, Chat: tg.Chat{ID: uid, Type: "private"}, Text: text,
	}})
}

func (h *harness) type_(text string) { h.say(testUser, text) }

func (h *harness) pressAs(uid int64, msgID int, data string) {
	h.upd++
	h.cbSeq++
	h.b.handleUpdate(h.ctx, tg.Update{UpdateID: h.upd, CallbackQuery: &tg.CallbackQuery{
		ID: "cb" + strconv.Itoa(h.cbSeq), From: tg.User{ID: uid}, Data: data,
		Message: &tg.Message{MessageID: msgID, Chat: tg.Chat{ID: uid, Type: "private"}},
	}})
}

func (h *harness) press(msgID int, data string) { h.pressAs(testUser, msgID, data) }

// pressLast presses a button on the most recently sent message.
func (h *harness) pressLast(data string) { h.press(h.tg.lastSentID(), data) }

func (h *harness) addAccount(name string, kind storage.AccountKind, cur string, balance int64) int64 {
	h.t.Helper()
	var id int64
	err := h.b.store.Update(testUser, func(u *storage.User) error {
		a := u.AddAccount(name, kind, cur, time.Now())
		id = a.ID
		if balance != 0 {
			u.AddTransaction(storage.Transaction{Type: storage.TxAdjust, AccountID: a.ID, Amount: balance}, time.Now())
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) balance(id int64) (v int64) {
	h.b.store.View(testUser, func(u *storage.User) { v = u.Balance(id) })
	return
}

func (h *harness) txs() (res []storage.Transaction) {
	h.b.store.View(testUser, func(u *storage.User) {
		for _, t := range u.Transactions {
			res = append(res, *t)
		}
	})
	return
}

// btnWithPrefix finds the callback data starting with prefix on the latest bot message.
func (h *harness) btnWithPrefix(prefix string) string {
	h.t.Helper()
	for _, d := range h.tg.last(h.t).buttons() {
		if strings.HasPrefix(d, prefix) {
			return d
		}
	}
	h.t.Fatalf("no button with prefix %q in %v", prefix, h.tg.last(h.t).buttons())
	return ""
}

// cat returns the category callback data for index idx on the latest draft.
func (h *harness) cat(idx int) string { return h.btnWithPrefix("d:c:" + strconv.Itoa(idx) + ":") }

func (h *harness) user(f func(u *storage.User)) { h.b.store.View(testUser, f) }

func contains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("text %q does not contain %q", got, want)
	}
}

// --- scenarios ---

func TestUnauthorizedUserGetsTheirID(t *testing.T) {
	h := newHarness(t, "")
	h.say(555, "/start")
	msgs := h.tg.byMethod("sendMessage")
	if len(msgs) != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", len(msgs))
	}
	contains(t, msgs[0].text(), "555")
	contains(t, msgs[0].text(), "ALLOWED_USER_IDS")
	h.user(func(u *storage.User) {})
	h.say(555, "350 кофе")
	if got := len(h.txs()); got != 0 {
		t.Errorf("unauthorized user created %d transactions", got)
	}
}

func TestUnauthorizedCallbackRejected(t *testing.T) {
	h := newHarness(t, "")
	h.pressAs(555, 1, "a:new")
	if got := h.tg.lastToast(t); !strings.Contains(got, "Доступ закрыт") {
		t.Errorf("toast = %q", got)
	}
	if n := len(h.tg.byMethod("editMessageText")) + len(h.tg.byMethod("sendMessage")); n != 0 {
		t.Errorf("unexpected messages: %d", n)
	}
}

func TestGroupChatsAreIgnored(t *testing.T) {
	h := newHarness(t, "")
	h.b.handleUpdate(h.ctx, tg.Update{Message: &tg.Message{
		From: &tg.User{ID: testUser}, Chat: tg.Chat{ID: -1, Type: "group"}, Text: "/start",
	}})
	if h.tg.count() != 0 {
		t.Errorf("bot reacted in a group chat: %d calls", h.tg.count())
	}
}

func TestStartWithoutAccountsOffersAddAccount(t *testing.T) {
	h := newHarness(t, "")
	h.type_("/start")
	msgs := h.tg.byMethod("sendMessage")
	if len(msgs) != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (menu + add account)", len(msgs))
	}
	if _, ok := msgs[0].Body["reply_markup"].(map[string]any)["keyboard"]; !ok {
		t.Error("first message must carry the reply keyboard")
	}
	if !msgs[1].hasButton("a:new") {
		t.Errorf("second message buttons = %v", msgs[1].buttons())
	}
}

func TestStartWithAccountShowsBalance(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Тинькофф", storage.KindBank, "RUB", 123450)
	h.type_("/start")
	last := h.tg.last(t)
	contains(t, last.text(), "Баланс")
	contains(t, last.text(), "Тинькофф")
	contains(t, last.text(), money.Format(123450, "RUB"))
}

func TestAddCashAccountViaCallbacks(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "a:new")
	if last := h.tg.last(t); !last.hasButton("a:k:cash") || !last.hasButton("a:k:bank") {
		t.Errorf("kind buttons = %v", last.buttons())
	}
	h.press(1, "a:k:cash")
	if !h.tg.last(t).hasButton("a:c:RUB") {
		t.Errorf("currency buttons = %v", h.tg.last(t).buttons())
	}
	h.press(1, "a:c:RUB")
	if !h.tg.last(t).hasButton("a:nd") {
		t.Errorf("default-name button missing: %v", h.tg.last(t).buttons())
	}
	h.press(1, "a:nd")
	if !h.tg.last(t).hasButton("a:b0") {
		t.Errorf("zero-balance button missing: %v", h.tg.last(t).buttons())
	}
	h.press(1, "a:b0")

	h.user(func(u *storage.User) {
		if len(u.Accounts) != 1 {
			t.Fatalf("accounts = %+v", u.Accounts)
		}
		a := u.Accounts[0]
		if a.Kind != storage.KindCash || a.Currency != "RUB" || a.Name != "Наличные "+money.Symbol("RUB") {
			t.Errorf("account = %+v", a)
		}
		if u.DefaultAccountID != a.ID {
			t.Error("first account must become default")
		}
		if len(u.Transactions) != 0 {
			t.Errorf("zero balance must not create a transaction: %+v", u.Transactions)
		}
	})
	if edits := h.tg.byMethod("editMessageText"); !strings.Contains(edits[len(edits)-1].text(), "Счёт добавлен") {
		t.Errorf("last edit = %q", edits[len(edits)-1].text())
	}
	// first account => reply keyboard "ready" message follows
	if got := h.tg.byMethod("sendMessage"); len(got) != 1 || got[0].Body["reply_markup"] == nil {
		t.Errorf("expected the menu message after first account, got %d sends", len(got))
	}
}

func TestAddBankAccountWithTypedNameAndBalance(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "a:new")
	h.press(1, "a:k:bank")
	h.press(1, "a:c:USD")
	h.type_("  Revolut   Main ")
	contains(t, h.tg.last(t).text(), "Сколько сейчас")
	h.type_("1 500,50")

	h.user(func(u *storage.User) {
		if len(u.Accounts) != 1 {
			t.Fatalf("accounts = %+v", u.Accounts)
		}
		a := u.Accounts[0]
		if a.Name != "Revolut Main" || a.Currency != "USD" || a.Kind != storage.KindBank {
			t.Errorf("account = %+v", a)
		}
		if u.Balance(a.ID) != 150050 {
			t.Errorf("balance = %d", u.Balance(a.ID))
		}
	})
}

func TestAddAccountWithCustomCurrency(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "a:new")
	h.press(1, "a:k:bank")
	h.press(1, "a:c:other")
	h.type_("не код!")
	contains(t, h.tg.last(t).text(), "Не похоже")
	h.type_("chf")
	h.type_("Swiss")
	h.type_("abc")
	contains(t, h.tg.last(t).text(), "Не понял сумму")
	h.type_("-15000")
	h.user(func(u *storage.User) {
		if len(u.Accounts) != 1 || u.Accounts[0].Currency != "CHF" || u.Balance(u.Accounts[0].ID) != -1500000 {
			t.Errorf("accounts = %+v bal=%v", u.Accounts, u.Balances())
		}
	})
}

func TestAddAccountStaleCallbacksDoNothing(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "a:b0") // no draft
	h.press(1, "a:nd")
	h.press(1, "a:k:bank")
	h.user(func(u *storage.User) {
		if len(u.Accounts) != 0 {
			t.Errorf("accounts created from stale callbacks: %+v", u.Accounts)
		}
	})
	contains(t, h.tg.last(t).text(), "устарело")
}

func TestQuickExpenseDecreasesBalance(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	h.type_("350 кофе")

	draft := h.tg.last(t)
	contains(t, draft.text(), money.Format(35000, "RUB"))
	contains(t, draft.text(), "кофе")
	if !draft.hasButton(h.cat(0)) {
		t.Fatalf("category buttons = %v", draft.buttons())
	}
	if len(h.txs()) != 1 {
		t.Fatal("draft must not be persisted before choosing category")
	}
	h.pressLast(h.cat(0))

	if got := h.balance(acc); got != 100000-35000 {
		t.Errorf("balance = %d, want %d", got, 100000-35000)
	}
	txs := h.txs()
	tx := txs[len(txs)-1]
	if tx.Type != storage.TxExpense || tx.Amount != 35000 || tx.Note != "кофе" || tx.Category != "🛒 Продукты" {
		t.Errorf("tx = %+v", tx)
	}
	last := h.tg.last(t)
	if last.Method != "editMessageText" {
		t.Errorf("result should edit the draft message, got %s", last.Method)
	}
	if !last.hasButton("u:" + strconv.FormatInt(tx.ID, 10)) {
		t.Errorf("undo button missing: %v", last.buttons())
	}
	if h.tg.lastToast(t) != "Записано" {
		t.Errorf("toast = %q", h.tg.lastToast(t))
	}
}

func TestQuickIncome(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_("+1000 зарплата")
	contains(t, h.tg.last(t).text(), "Доход")
	h.pressLast(h.cat(0))
	if got := h.balance(acc); got != 100000 {
		t.Errorf("balance = %d, want 100000", got)
	}
	txs := h.txs()
	if len(txs) != 1 || txs[0].Type != storage.TxIncome || txs[0].Category != "💼 Зарплата" {
		t.Errorf("txs = %+v", txs)
	}
}

func TestQuickEntryPicksAccountByCurrency(t *testing.T) {
	h := newHarness(t, "")
	rub := h.addAccount("Rub", storage.KindBank, "RUB", 100000)
	usd := h.addAccount("Usd", storage.KindCash, "USD", 10000)
	h.type_("20$ такси")
	contains(t, h.tg.last(t).text(), money.Format(2000, "USD"))
	h.pressLast(h.cat(2))
	if got := h.balance(usd); got != 8000 {
		t.Errorf("usd balance = %d, want 8000", got)
	}
	if got := h.balance(rub); got != 100000 {
		t.Errorf("rub balance changed to %d", got)
	}
}

func TestQuickEntryUnknownCurrencyAccount(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Rub", storage.KindBank, "RUB", 100)
	h.type_("20 eur обед")
	last := h.tg.last(t)
	contains(t, last.text(), "нет счёта в EUR")
	if !last.hasButton("a:new") {
		t.Errorf("buttons = %v", last.buttons())
	}
	if len(h.txs()) != 1 {
		t.Error("transaction created")
	}
}

func TestQuickEntryWithoutAccountsAsksForAccount(t *testing.T) {
	h := newHarness(t, "")
	h.type_("350 кофе")
	if !h.tg.last(t).hasButton("a:new") {
		t.Errorf("buttons = %v", h.tg.last(t).buttons())
	}
}

func TestGarbageTextGetsHelp(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_("привет")
	contains(t, h.tg.last(t).text(), "Не понял")
	if len(h.txs()) != 0 {
		t.Error("transaction created from garbage")
	}
}

func TestChangeAccountAndNoteBeforeSaving(t *testing.T) {
	h := newHarness(t, "")
	rub := h.addAccount("Rub", storage.KindBank, "RUB", 0)
	cash := h.addAccount("Cash", storage.KindCash, "RUB", 50000)
	h.type_("100") // 100.00 -> 10000 minor units
	id := h.tg.lastSentID()
	h.press(id, "d:a")
	if !h.tg.last(t).hasButton("d:s:" + strconv.FormatInt(cash, 10)) {
		t.Fatalf("buttons = %v", h.tg.last(t).buttons())
	}
	h.press(id, "d:s:"+strconv.FormatInt(cash, 10))
	h.press(id, "d:n")
	h.type_("  мой   комментарий ")
	// note re-sends the draft, so a new message id is current
	if len(h.tg.byMethod("deleteMessage")) != 1 {
		t.Error("old draft message should be deleted")
	}
	newID := h.tg.lastSentID()
	if newID == id {
		t.Fatal("draft was not re-sent")
	}
	cat0 := h.cat(0)
	h.press(id, cat0) // old message is stale now
	if len(h.txs()) != 1 {
		t.Fatal("stale press saved a transaction")
	}
	h.press(newID, cat0)
	txs := h.txs()
	tx := txs[len(txs)-1]
	if tx.AccountID != cash || tx.Note != "мой комментарий" {
		t.Errorf("tx = %+v", tx)
	}
	if h.balance(cash) != 40000 || h.balance(rub) != 0 {
		t.Errorf("balances = %d %d", h.balance(cash), h.balance(rub))
	}
}

func TestCancelDraft(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_("100 x")
	h.pressLast("d:x")
	contains(t, h.tg.last(t).text(), "отменена")
	h.pressLast("d:c:0:x")
	if len(h.txs()) != 0 {
		t.Error("cancelled draft was saved")
	}
}

func TestExpenseViaMenuButton(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 10000)
	h.type_(btnExpense)
	contains(t, h.tg.last(t).text(), "Введите сумму")
	h.type_("12,5 хлеб")
	h.pressLast(h.cat(0))
	if got := h.balance(acc); got != 10000-1250 {
		t.Errorf("balance = %d", got)
	}
}

func TestMenuButtonWithoutAccount(t *testing.T) {
	h := newHarness(t, "")
	h.type_(btnExpense)
	if !h.tg.last(t).hasButton("a:new") {
		t.Errorf("buttons = %v", h.tg.last(t).buttons())
	}
	h.type_(btnTransfer)
	contains(t, h.tg.last(t).text(), "минимум два счёта")
}

func TestUndo(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	h.type_("350 кофе")
	h.pressLast(h.cat(0))
	txs := h.txs()
	id := txs[len(txs)-1].ID
	if h.balance(acc) != 65000 {
		t.Fatalf("balance = %d", h.balance(acc))
	}
	h.pressLast("u:" + strconv.FormatInt(id, 10))
	if got := h.balance(acc); got != 100000 {
		t.Errorf("balance after undo = %d", got)
	}
	contains(t, h.tg.last(t).text(), "отменена")
	// second undo is harmless
	h.pressLast("u:" + strconv.FormatInt(id, 10))
	contains(t, h.tg.last(t).text(), "уже удалена")
	if got := h.balance(acc); got != 100000 {
		t.Errorf("balance after double undo = %d", got)
	}
}

func TestUndoUnknownOrGarbageID(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100)
	h.press(1, "u:99999")
	h.press(1, "u:abc")
	h.press(1, "u")
	if h.balance(acc) != 100 || len(h.txs()) != 1 {
		t.Error("state changed")
	}
}

func TestStaleDraftCallbackAfterNewDraft(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	h.type_("100 первый")
	first := h.tg.lastSentID()
	h.type_("200 второй")
	second := h.tg.lastSentID()
	cat0 := h.cat(0)
	if first == second {
		t.Fatal("expected a new message for the second draft")
	}

	h.press(first, cat0)
	if h.balance(acc) != 100000 || len(h.txs()) != 1 {
		t.Fatal("stale callback was applied")
	}
	if got := h.tg.lastToast(t); !strings.Contains(got, "устарело") {
		t.Errorf("toast = %q", got)
	}

	h.press(second, cat0)
	if got := h.balance(acc); got != 100000-20000 {
		t.Errorf("balance = %d, want %d", got, 100000-20000)
	}
	txs := h.txs()
	if txs[len(txs)-1].Note != "второй" {
		t.Errorf("saved wrong draft: %+v", txs[len(txs)-1])
	}
}

func TestDraftCallbackWithoutDraftIsStale(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.press(7, "d:c:0:x")
	if got := h.tg.lastToast(t); !strings.Contains(got, "устарело") {
		t.Errorf("toast = %q", got)
	}
}

func TestDraftCategoryOutOfRangeDoesNotSave(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_("100 x")
	h.pressLast("d:c:999:zzz")
	if len(h.txs()) != 0 {
		t.Error("saved with invalid category index")
	}
}

func TestDraftSurvivesFailedEditByResending(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	h.type_("100 x")
	h.tg.failEdit = true
	h.pressLast(h.cat(0))
	if h.balance(acc) != 90000 {
		t.Errorf("balance = %d", h.balance(acc))
	}
	// fallback to sendMessage with the result
	contains(t, h.tg.last(t).text(), "Расход")
	if h.tg.last(t).Method != "sendMessage" {
		t.Errorf("method = %s", h.tg.last(t).Method)
	}
}

func transferSetup(t *testing.T, ratesURL string) (*harness, int64, int64) {
	h := newHarness(t, ratesURL)
	rub := h.addAccount("Rub", storage.KindBank, "RUB", 1000000)
	usd := h.addAccount("Usd", storage.KindBank, "USD", 0)
	return h, rub, usd
}

func startTransfer(h *harness, from, to int64) {
	h.type_(btnTransfer)
	id := h.tg.lastSentID()
	h.press(id, "tf:f:"+strconv.FormatInt(from, 10))
	h.press(id, "tf:t:"+strconv.FormatInt(to, 10))
}

func TestTransferSameCurrency(t *testing.T) {
	h := newHarness(t, "")
	a := h.addAccount("Card", storage.KindBank, "RUB", 1000000)
	c := h.addAccount("Cash", storage.KindCash, "RUB", 0)
	startTransfer(h, a, c)
	h.type_("5000 снял")
	if h.balance(a) != 500000 || h.balance(c) != 500000 {
		t.Errorf("balances = %d %d", h.balance(a), h.balance(c))
	}
	txs := h.txs()
	tx := txs[len(txs)-1]
	if tx.Type != storage.TxTransfer || tx.Amount != 500000 || tx.ToAmount != 500000 || tx.Note != "снял" {
		t.Errorf("tx = %+v", tx)
	}
}

func TestTransferCrossCurrencyManualSecondAmount(t *testing.T) {
	h, rub, usd := transferSetup(t, "")
	startTransfer(h, rub, usd)
	h.type_("9500")
	last := h.tg.last(t)
	contains(t, last.text(), "Сколько поступило")
	if last.hasButton("tf:r") {
		t.Error("rate button shown although rates are disabled")
	}
	if len(h.txs()) != 1 {
		t.Fatal("transfer saved before second amount")
	}
	h.type_("не число")
	if len(h.txs()) != 1 {
		t.Fatal("transfer saved on invalid second amount")
	}
	h.type_("100")
	if h.balance(rub) != 1000000-950000 || h.balance(usd) != 10000 {
		t.Errorf("balances rub=%d usd=%d", h.balance(rub), h.balance(usd))
	}
	txs := h.txs()
	tx := txs[len(txs)-1]
	if tx.Amount != 950000 || tx.ToAmount != 10000 || tx.AccountID != rub || tx.ToAccountID != usd {
		t.Errorf("tx = %+v", tx)
	}
	contains(t, h.tg.last(t).text(), "Курс")
}

func TestTransferCrossCurrencyRejectsNonPositiveSecondAmount(t *testing.T) {
	h, rub, usd := transferSetup(t, "")
	startTransfer(h, rub, usd)
	h.type_("900")
	h.type_("0")
	h.type_("-5")
	if len(h.txs()) != 1 {
		t.Errorf("transfer saved with non-positive amount: %+v", h.txs())
	}
}

func TestTransferCrossCurrencyUsesRateButton(t *testing.T) {
	h, rub, usd := transferSetup(t, ratesServer(t))
	startTransfer(h, rub, usd)
	h.type_("9000")
	last := h.tg.last(t)
	if !last.hasButton("tf:r") {
		t.Fatalf("rate button missing: %v", last.buttons())
	}
	contains(t, last.text(), money.Format(10000, "USD"))
	h.pressLast("tf:r")
	if h.balance(usd) != 10000 || h.balance(rub) != 1000000-900000 {
		t.Errorf("balances rub=%d usd=%d", h.balance(rub), h.balance(usd))
	}
}

func TestTransferUndo(t *testing.T) {
	h, rub, usd := transferSetup(t, "")
	startTransfer(h, rub, usd)
	h.type_("900")
	h.type_("10")
	txs := h.txs()
	id := txs[len(txs)-1].ID
	h.pressLast("u:" + strconv.FormatInt(id, 10))
	if h.balance(rub) != 1000000 || h.balance(usd) != 0 {
		t.Errorf("balances rub=%d usd=%d", h.balance(rub), h.balance(usd))
	}
}

func TestTransferCancelAndBack(t *testing.T) {
	h, rub, usd := transferSetup(t, "")
	h.type_(btnTransfer)
	id := h.tg.lastSentID()
	h.press(id, "tf:f:"+strconv.FormatInt(rub, 10))
	contains(t, h.tg.last(t).text(), "Откуда: ")
	h.press(id, "tf:back")
	contains(t, h.tg.last(t).text(), "Откуда?")
	h.press(id, "tf:f:"+strconv.FormatInt(usd, 10))
	h.press(id, "tf:x")
	contains(t, h.tg.last(t).text(), "отменён")
	h.type_("100") // now treated as quick expense, not a transfer
	if h.balance(rub) != 1000000 {
		t.Error("balance changed")
	}
}

func TestTransferToSameAccountIsRejected(t *testing.T) {
	h, rub, _ := transferSetup(t, "")
	h.type_(btnTransfer)
	id := h.tg.lastSentID()
	h.press(id, "tf:f:"+strconv.FormatInt(rub, 10))
	h.press(id, "tf:t:"+strconv.FormatInt(rub, 10))
	h.type_("100")
	for _, tx := range h.txs() {
		if tx.Type == storage.TxTransfer {
			t.Errorf("transfer to the same account was saved: %+v", tx)
		}
	}
}

func TestTransferStaleCallback(t *testing.T) {
	h, rub, usd := transferSetup(t, "")
	h.type_(btnTransfer)
	old := h.tg.lastSentID()
	h.type_(btnTransfer)
	h.press(old, "tf:f:"+strconv.FormatInt(rub, 10))
	if got := h.tg.lastToast(t); !strings.Contains(got, "устарело") {
		t.Errorf("toast = %q", got)
	}
	_ = usd
}

func seedHistory(h *harness, acc int64, n int) {
	h.t.Helper()
	err := h.b.store.Update(testUser, func(u *storage.User) error {
		for i := 1; i <= n; i++ {
			u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: acc, Amount: int64(i) * 100, Note: "n" + strconv.Itoa(i)}, time.Now())
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func TestHistoryPagination(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 0)
	seedHistory(h, acc, 25)

	h.type_(btnHistory)
	id := h.tg.lastSentID()
	p1 := h.tg.last(t)
	// newest first
	contains(t, p1.text(), "1. ")
	contains(t, p1.text(), "n25")
	contains(t, p1.text(), "10. ")
	contains(t, p1.text(), "n16")
	if strings.Contains(p1.text(), "11. ") || strings.Contains(p1.text(), "n15") {
		t.Errorf("first page leaks later items: %s", p1.text())
	}
	if !p1.hasButton("h:10:0") {
		t.Errorf("next button missing: %v", p1.buttons())
	}
	for _, d := range p1.buttons() {
		if strings.HasPrefix(d, "h:") && d != "h:10:0" {
			t.Errorf("first page must not have prev button, got %s", d)
		}
	}

	h.press(id, "h:10:0")
	p2 := h.tg.last(t)
	contains(t, p2.text(), "11. ")
	contains(t, p2.text(), "n15")
	contains(t, p2.text(), "20. ")
	if !p2.hasButton("h:20:0") || !p2.hasButton("h:0:0") {
		t.Errorf("page 2 buttons = %v", p2.buttons())
	}

	h.press(id, "h:20:0")
	p3 := h.tg.last(t)
	contains(t, p3.text(), "21. ")
	contains(t, p3.text(), "n1")
	if strings.Contains(p3.text(), "26. ") {
		t.Error("page 3 has too many items")
	}
	if p3.hasButton("h:30:0") {
		t.Error("last page must not have next button")
	}
	if !p3.hasButton("h:10:0") {
		t.Errorf("page 3 buttons = %v", p3.buttons())
	}

	// out of range offset snaps to the last page
	h.press(id, "h:999:0")
	contains(t, h.tg.last(t).text(), "21. ")
}

func TestHistoryEmpty(t *testing.T) {
	h := newHarness(t, "")
	h.type_(btnHistory)
	contains(t, h.tg.last(t).text(), "Операций пока нет")
}

func TestHistoryOpenAndDeleteTransaction(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	seedHistory(h, acc, 3)
	before := h.balance(acc)
	txs := h.txs()
	target := txs[len(txs)-1] // newest, 300
	h.type_(btnHistory)
	id := h.tg.lastSentID()
	tid := strconv.FormatInt(target.ID, 10)
	if !h.tg.last(t).hasButton("tx:" + tid + ":0:0") {
		t.Fatalf("buttons = %v", h.tg.last(t).buttons())
	}
	h.press(id, "tx:"+tid+":0:0")
	if !h.tg.last(t).hasButton("txd:" + tid + ":0:0") {
		t.Fatalf("delete button missing: %v", h.tg.last(t).buttons())
	}
	h.press(id, "txd:"+tid+":0:0")
	if got := h.balance(acc); got != before+target.Amount {
		t.Errorf("balance = %d, want %d", got, before+target.Amount)
	}
	if len(h.txs()) != 3 { // adjust + 2 seeded
		t.Errorf("txs = %d", len(h.txs()))
	}
	h.press(id, "txd:"+tid+":0:0")
	contains(t, h.tg.last(t).text(), "уже удалена")
}

func TestAccountDeletionWithTransactionsArchives(t *testing.T) {
	h := newHarness(t, "")
	a := h.addAccount("A", storage.KindBank, "RUB", 100000)
	b := h.addAccount("B", storage.KindBank, "RUB", 0)
	sa := strconv.FormatInt(a, 10)

	h.press(1, "a:del:"+sa)
	contains(t, h.tg.last(t).text(), "операции по нему останутся")
	h.press(1, "a:dy:"+sa)

	h.user(func(u *storage.User) {
		acc := u.Account(a)
		if acc == nil || !acc.Archived {
			t.Errorf("account A should be archived, got %+v", acc)
		}
		if u.DefaultAccountID != b {
			t.Errorf("default = %d, want %d", u.DefaultAccountID, b)
		}
		if len(u.Transactions) != 1 {
			t.Errorf("transactions lost: %d", len(u.Transactions))
		}
	})
	last := h.tg.last(t)
	if last.hasButton("a:v:" + sa) {
		t.Error("archived account still listed")
	}
	if !last.hasButton("a:v:" + strconv.FormatInt(b, 10)) {
		t.Error("remaining account missing from list")
	}
	// archived account no longer affects quick entry
	h.type_("10 x")
	h.pressLast(h.cat(0))
	if h.balance(b) != -1000 {
		t.Errorf("expense went to wrong account: a=%d b=%d", h.balance(a), h.balance(b))
	}
	// history still readable with the archived account
	h.type_(btnHistory)
	contains(t, h.tg.last(t).text(), "A")
	// deleting it again reports gone
	h.press(1, "a:dy:"+sa)
	contains(t, h.tg.last(t).text(), "не найден")
}

func TestAccountDeletionWithoutTransactionsRemoves(t *testing.T) {
	h := newHarness(t, "")
	a := h.addAccount("A", storage.KindBank, "RUB", 0)
	h.press(1, "a:dy:"+strconv.FormatInt(a, 10))
	h.user(func(u *storage.User) {
		if len(u.Accounts) != 0 || u.DefaultAccountID != 0 {
			t.Errorf("account not removed: %+v default=%d", u.Accounts, u.DefaultAccountID)
		}
	})
}

func TestMakeDefaultAccount(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("A", storage.KindBank, "RUB", 0)
	b := h.addAccount("B", storage.KindBank, "RUB", 0)
	h.press(1, "a:def:"+strconv.FormatInt(b, 10))
	h.user(func(u *storage.User) {
		if u.DefaultAccountID != b {
			t.Errorf("default = %d", u.DefaultAccountID)
		}
	})
	h.press(1, "a:def:9999")
	h.user(func(u *storage.User) {
		if u.DefaultAccountID != b {
			t.Errorf("default changed to unknown account: %d", u.DefaultAccountID)
		}
	})
}

func TestRenameAndAdjustAccount(t *testing.T) {
	h := newHarness(t, "")
	a := h.addAccount("Old", storage.KindBank, "RUB", 100000)
	sa := strconv.FormatInt(a, 10)
	h.press(1, "a:rn:"+sa)
	h.type_("New name")
	h.user(func(u *storage.User) {
		if u.Account(a).Name != "New name" {
			t.Errorf("name = %q", u.Account(a).Name)
		}
	})
	h.press(1, "a:adj:"+sa)
	h.type_("1200")
	if got := h.balance(a); got != 120000 {
		t.Errorf("balance = %d, want 120000", got)
	}
	txs := h.txs()
	if last := txs[len(txs)-1]; last.Type != storage.TxAdjust || last.Amount != 20000 {
		t.Errorf("adjust tx = %+v", last)
	}
	// same balance => no new transaction
	n := len(h.txs())
	h.press(1, "a:adj:"+sa)
	h.type_("1200")
	if len(h.txs()) != n {
		t.Error("no-op adjust created a transaction")
	}
}

func TestReportShowsTotals(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 0)
	err := h.b.store.Update(testUser, func(u *storage.User) error {
		now := time.Now()
		u.AddTransaction(storage.Transaction{Type: storage.TxIncome, AccountID: acc, Amount: 100000, Category: "💼 Зарплата"}, now)
		u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: acc, Amount: 35000, Category: "🍽 Кафе"}, now)
		u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: acc, Amount: 15000, Category: "🛒 Продукты"}, now)
		// previous month: must be excluded
		u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: acc, Amount: 999900, Category: "📦 Другое"}, now.AddDate(0, -1, 0))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.type_(btnReport)
	txt := h.tg.last(t).text()
	contains(t, txt, "Доходы: <b>"+money.Format(100000, "RUB"))
	contains(t, txt, "Расходы: <b>"+money.Format(50000, "RUB"))
	contains(t, txt, "Итог: <b>"+money.FormatSigned(50000, "RUB"))
	contains(t, txt, "Кафе")
	contains(t, txt, "70%")
	if strings.Contains(txt, "Другое") {
		t.Error("previous month leaked into the report")
	}
	if !h.tg.last(t).hasButton("r:" + time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")) {
		t.Errorf("prev month button missing: %v", h.tg.last(t).buttons())
	}
}

func TestReportConvertsCurrencies(t *testing.T) {
	h := newHarness(t, ratesServer(t))
	rub := h.addAccount("Rub", storage.KindBank, "RUB", 0)
	usd := h.addAccount("Usd", storage.KindBank, "USD", 0)
	_ = h.b.store.Update(testUser, func(u *storage.User) error {
		now := time.Now()
		u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: rub, Amount: 100000}, now)
		u.AddTransaction(storage.Transaction{Type: storage.TxExpense, AccountID: usd, Amount: 1000}, now) // 10 USD = 900 RUB
		return nil
	})
	h.type_(btnReport)
	contains(t, h.tg.last(t).text(), "Расходы: <b>"+money.Format(100000+90000, "RUB"))
}

func TestReportEmptyMonthAndBadMonthCallback(t *testing.T) {
	h := newHarness(t, "")
	h.type_(btnReport)
	contains(t, h.tg.last(t).text(), "операций нет")
	h.press(1, "r:2020-01")
	contains(t, h.tg.last(t).text(), "Январь 2020")
	h.press(1, "r:garbage")
	contains(t, h.tg.last(t).text(), strconv.Itoa(time.Now().Year()))
}

func TestBalanceWithRatesShowsTotal(t *testing.T) {
	h := newHarness(t, ratesServer(t))
	h.addAccount("Rub", storage.KindBank, "RUB", 900000)
	h.addAccount("Usd", storage.KindBank, "USD", 10000) // 100 USD = 9000 RUB
	h.type_(btnBalance)
	contains(t, h.tg.last(t).text(), "Всего ≈ <b>"+money.Format(1800000, "RUB"))
}

func TestBalanceRatesUnavailable(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Rub", storage.KindBank, "RUB", 900000)
	h.addAccount("Usd", storage.KindBank, "USD", 10000)
	h.type_(btnBalance)
	txt := h.tg.last(t).text()
	// RUB is the base currency and converts 1:1; USD has no rate and is listed separately.
	contains(t, txt, "без учёта (нет курса): "+money.Format(10000, "USD"))
}

func TestCancelCommandResetsPendingInput(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_(btnExpense)
	h.type_("/cancel")
	h.type_("не сумма")
	contains(t, h.tg.last(t).text(), "Не понял")
}

func TestCommandWithBotSuffix(t *testing.T) {
	h := newHarness(t, "")
	h.type_("/start@financer_bot")
	if len(h.tg.byMethod("sendMessage")) == 0 {
		t.Error("no response to /start@bot")
	}
}

func TestHTMLEscapingOfUserText(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_("100 <b>x</b> & y")
	txt := h.tg.last(t).text()
	if strings.Contains(txt, "<b>x</b>") {
		t.Errorf("user HTML not escaped: %s", txt)
	}
	contains(t, txt, "&lt;b&gt;x&lt;/b&gt; &amp; y")
}

func TestSettingsCategoriesAddAndDelete(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "s:ca:e")
	h.type_("🐶 Питомец")
	h.user(func(u *storage.User) {
		cats := u.Categories(storage.TxExpense)
		if cats[len(cats)-1] != "🐶 Питомец" {
			t.Errorf("cats = %v", cats)
		}
	})
	h.press(1, "s:ca:e")
	h.type_("🐶 питомец") // duplicate, case-insensitive
	contains(t, h.tg.last(t).text(), "уже есть")
	var n int
	h.user(func(u *storage.User) { n = len(u.ExpenseCategories) })
	h.press(1, "s:c:e")
	del0 := h.btnWithPrefix("s:cd:e:0:")
	h.press(1, "s:cd:e:0:wronghash") // stale hash must not delete
	h.user(func(u *storage.User) {
		if len(u.ExpenseCategories) != n {
			t.Errorf("category deleted with wrong hash")
		}
	})
	h.press(1, del0)
	h.user(func(u *storage.User) {
		if len(u.ExpenseCategories) != n-1 {
			t.Errorf("category not deleted")
		}
	})
	h.press(1, "s:cd:e:999:zzz")
	h.user(func(u *storage.User) {
		if len(u.ExpenseCategories) != n-1 {
			t.Errorf("out-of-range delete changed categories")
		}
	})
}

func TestSettingsBaseCurrency(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "s:bc:EUR")
	h.user(func(u *storage.User) {
		if u.BaseCurrency != "EUR" {
			t.Errorf("base = %q", u.BaseCurrency)
		}
	})
	h.press(1, "s:bc:bad!")
	h.user(func(u *storage.User) {
		if u.BaseCurrency != "EUR" {
			t.Errorf("invalid code accepted: %q", u.BaseCurrency)
		}
	})
}

func TestExportCSV(t *testing.T) {
	h := newHarness(t, "")
	h.press(1, "s:x")
	contains(t, h.tg.last(t).text(), "нечего выгружать")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100)
	_ = acc
	h.press(1, "s:x")
	// sendDocument is multipart and not JSON; it is recorded with an empty body.
	if len(h.tg.byMethod("sendDocument")) != 1 {
		t.Errorf("sendDocument calls = %d", len(h.tg.byMethod("sendDocument")))
	}
}

func TestSafeCell(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "=SUM(A1)": "'=SUM(A1)", "+1": "'+1", "-1": "'-1", "@x": "'@x"} {
		if got := safeCell(in); got != want {
			t.Errorf("safeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCallbackAlwaysAnswered(t *testing.T) {
	h := newHarness(t, "")
	h.pressAs(555, 1, "x")
	h.press(1, "zzz:unknown")
	h.press(1, "d")
	if got := len(h.tg.byMethod("answerCallbackQuery")); got != 3 {
		t.Errorf("answerCallbackQuery calls = %d, want 3", got)
	}
}

// catCB строит callback категории с отпечатком названия, как это делает бот.
func (h *harness) catCB(prefix string, typ storage.TxType, idx int) string {
	cb := prefix + ":" + strconv.Itoa(idx)
	h.user(func(u *storage.User) {
		if cats := u.Categories(typ); idx < len(cats) {
			cb += ":" + catHash(cats[idx])
		}
	})
	return cb
}

func TestDraftCategoryWithWrongHashIsStale(t *testing.T) {
	h := newHarness(t, "")
	acc := h.addAccount("Card", storage.KindBank, "RUB", 100000)
	h.type_("100 x")
	h.pressLast("d:c:0:wronghash")
	if h.balance(acc) != 100000 || len(h.txs()) != 1 {
		t.Error("callback with wrong hash was applied")
	}
	h.pressLast("d:c:0") // legacy format without hash
	if len(h.txs()) != 1 {
		t.Error("callback without hash was applied")
	}
	// the draft is still usable afterwards? it is stale by contract; just ensure no panic
}

func TestUnknownCommandWhileAwaitingInputIsNotConsumed(t *testing.T) {
	h := newHarness(t, "")
	h.addAccount("Card", storage.KindBank, "RUB", 0)
	h.type_(btnExpense)
	h.type_("/foo")
	contains(t, h.tg.last(t).text(), "Неизвестная команда")
	if len(h.txs()) != 0 {
		t.Error("transaction created from command")
	}
	h.type_("50 x") // still awaiting amount
	contains(t, h.tg.last(t).text(), "Выберите категорию")
}
