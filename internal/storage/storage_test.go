package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

func TestBalances(t *testing.T) {
	u := newUser(1)
	rub := u.AddAccount("Card", KindBank, "RUB", t0)
	usd := u.AddAccount("Dollars", KindCash, "USD", t0)
	empty := u.AddAccount("Empty", KindCash, "EUR", t0)

	u.AddTransaction(Transaction{Type: TxAdjust, AccountID: rub.ID, Amount: 100000}, t0)
	u.AddTransaction(Transaction{Type: TxAdjust, AccountID: usd.ID, Amount: 5000}, t0)
	u.AddTransaction(Transaction{Type: TxExpense, AccountID: rub.ID, Amount: 35000}, t0)
	u.AddTransaction(Transaction{Type: TxIncome, AccountID: rub.ID, Amount: 10000}, t0)
	u.AddTransaction(Transaction{Type: TxTransfer, AccountID: rub.ID, Amount: 9000, ToAccountID: usd.ID, ToAmount: 100}, t0)
	u.AddTransaction(Transaction{Type: TxAdjust, AccountID: usd.ID, Amount: -50}, t0)

	b := u.Balances()
	if got, want := b[rub.ID], int64(100000-35000+10000-9000); got != want {
		t.Errorf("rub = %d, want %d", got, want)
	}
	if got, want := b[usd.ID], int64(5000+100-50); got != want {
		t.Errorf("usd = %d, want %d", got, want)
	}
	if got := u.Balance(empty.ID); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
}

func TestBalancesTransferSameCurrencyConserves(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindCash, "RUB", t0)
	u.AddTransaction(Transaction{Type: TxAdjust, AccountID: a.ID, Amount: 1000}, t0)
	u.AddTransaction(Transaction{Type: TxTransfer, AccountID: a.ID, Amount: 400, ToAccountID: b.ID, ToAmount: 400}, t0)
	bal := u.Balances()
	if bal[a.ID] != 600 || bal[b.ID] != 400 {
		t.Errorf("balances = %v", bal)
	}
}

func TestDeleteTransactionRestoresBalance(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "USD", t0)
	u.AddTransaction(Transaction{Type: TxAdjust, AccountID: a.ID, Amount: 1000}, t0)
	tr := u.AddTransaction(Transaction{Type: TxTransfer, AccountID: a.ID, Amount: 900, ToAccountID: b.ID, ToAmount: 10}, t0)
	if u.Balance(a.ID) != 100 || u.Balance(b.ID) != 10 {
		t.Fatalf("unexpected balances before delete")
	}
	if !u.DeleteTransaction(tr.ID) {
		t.Fatal("DeleteTransaction returned false")
	}
	if u.Balance(a.ID) != 1000 || u.Balance(b.ID) != 0 {
		t.Errorf("balances after delete = %v", u.Balances())
	}
	if u.Transaction(tr.ID) != nil {
		t.Error("transaction still present")
	}
	if u.DeleteTransaction(tr.ID) {
		t.Error("second delete should return false")
	}
	if u.DeleteTransaction(9999) {
		t.Error("deleting unknown id should return false")
	}
}

func TestAddAccountAssignsIDsAndDefault(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "USD", t0)
	if a.ID == b.ID || a.ID == 0 {
		t.Errorf("ids not unique: %d %d", a.ID, b.ID)
	}
	if u.DefaultAccountID != a.ID {
		t.Errorf("default = %d, want first account %d", u.DefaultAccountID, a.ID)
	}
	tx := u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 1}, t0)
	if tx.ID == a.ID || tx.ID == b.ID {
		t.Errorf("transaction id %d collides with account ids", tx.ID)
	}
	if !tx.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v", tx.CreatedAt)
	}
}

func TestNextIDRecoversFromZero(t *testing.T) {
	u := &User{}
	if id := u.nextID(); id != 1 {
		t.Errorf("id = %d, want 1", id)
	}
}

func TestRemoveAccountDeletesWhenNoTransactions(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	if archived := u.RemoveAccount(a.ID); archived {
		t.Error("archived = true, want false")
	}
	if u.Account(a.ID) != nil || len(u.Accounts) != 0 {
		t.Errorf("account not deleted: %+v", u.Accounts)
	}
	if u.DefaultAccountID != 0 || u.DefaultAccount() != nil {
		t.Errorf("default should be cleared, got %d", u.DefaultAccountID)
	}
}

func TestRemoveAccountArchivesWhenHasTransactions(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "RUB", t0)
	u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 100}, t0)

	if archived := u.RemoveAccount(a.ID); !archived {
		t.Fatal("archived = false, want true")
	}
	if u.Account(a.ID) == nil {
		t.Error("archived account must remain findable")
	}
	if u.ActiveAccount(a.ID) != nil {
		t.Error("archived account must not be active")
	}
	if got := u.ActiveAccounts(); len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("active accounts = %v", got)
	}
	if u.DefaultAccountID != b.ID {
		t.Errorf("default = %d, want %d", u.DefaultAccountID, b.ID)
	}
}

func TestRemoveAccountReferencedAsTransferTargetArchives(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "RUB", t0)
	u.AddTransaction(Transaction{Type: TxTransfer, AccountID: a.ID, Amount: 5, ToAccountID: b.ID, ToAmount: 5}, t0)
	if !u.RemoveAccount(b.ID) {
		t.Error("expected archive for transfer target")
	}
}

func TestRemoveNonDefaultAccountKeepsDefault(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "RUB", t0)
	u.RemoveAccount(b.ID)
	if u.DefaultAccountID != a.ID {
		t.Errorf("default = %d, want %d", u.DefaultAccountID, a.ID)
	}
}

func TestRemoveUnknownAccountIsNoop(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	if u.RemoveAccount(12345) {
		t.Error("archived = true for unknown id")
	}
	if len(u.Accounts) != 1 || u.DefaultAccountID != a.ID {
		t.Error("state changed")
	}
}

func TestAddAccountAfterArchivingDefaultTakesOverDefault(t *testing.T) {
	u := newUser(1)
	a := u.AddAccount("A", KindBank, "RUB", t0)
	u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 1}, t0)
	u.RemoveAccount(a.ID)
	b := u.AddAccount("B", KindBank, "RUB", t0)
	if u.DefaultAccountID != b.ID {
		t.Errorf("default = %d, want %d", u.DefaultAccountID, b.ID)
	}
}

func TestDefaultAccountFallbacks(t *testing.T) {
	u := newUser(1)
	if u.DefaultAccount() != nil {
		t.Error("no accounts: want nil")
	}
	a := u.AddAccount("A", KindBank, "RUB", t0)
	b := u.AddAccount("B", KindBank, "USD", t0)
	if u.DefaultAccount() != a {
		t.Error("want first account")
	}
	u.DefaultAccountID = b.ID
	if u.DefaultAccount() != b {
		t.Error("want explicit default")
	}
	// dangling default id falls back to first active
	u.DefaultAccountID = 777
	if u.DefaultAccount() != a {
		t.Error("dangling default should fall back to first active")
	}
	// archived default falls back too
	u.DefaultAccountID = a.ID
	a.Archived = true
	if u.DefaultAccount() != b {
		t.Error("archived default should fall back to first active")
	}
}

func TestAccountByCurrency(t *testing.T) {
	u := newUser(1)
	rub := u.AddAccount("Rub", KindBank, "RUB", t0)
	usd1 := u.AddAccount("Usd1", KindBank, "USD", t0)
	usd2 := u.AddAccount("Usd2", KindCash, "USD", t0)

	if got := u.AccountByCurrency("RUB"); got != rub {
		t.Errorf("RUB: got %v", got)
	}
	if got := u.AccountByCurrency("USD"); got != usd1 {
		t.Errorf("USD: want first usd account, got %v", got)
	}
	u.DefaultAccountID = usd2.ID
	if got := u.AccountByCurrency("USD"); got != usd2 {
		t.Errorf("USD: want default preferred, got %v", got)
	}
	usd1.Archived, usd2.Archived = true, true
	if got := u.AccountByCurrency("USD"); got != nil {
		t.Errorf("archived accounts must be ignored, got %v", got)
	}
	if got := u.AccountByCurrency("EUR"); got != nil {
		t.Errorf("EUR: want nil, got %v", got)
	}
}

func TestCategories(t *testing.T) {
	u := newUser(1)
	if len(u.Categories(TxExpense)) == 0 || len(u.Categories(TxIncome)) == 0 {
		t.Fatal("defaults missing")
	}
	u.SetCategories(TxIncome, []string{"x"})
	if got := u.Categories(TxIncome); len(got) != 1 || got[0] != "x" {
		t.Errorf("income = %v", got)
	}
	if len(u.Categories(TxExpense)) == 1 {
		t.Error("expense categories must be unaffected")
	}
}

func TestNewUserDoesNotShareDefaultCategories(t *testing.T) {
	a, b := newUser(1), newUser(2)
	a.ExpenseCategories[0] = "changed"
	if b.ExpenseCategories[0] == "changed" || defaultExpenseCategories[0] == "changed" {
		t.Error("default categories slice is shared")
	}
}

// --- Store ---

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sub", "data.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func reopen(t *testing.T, s *Store, p string) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	return s2
}

func TestOpenMissingFileStartsEmpty(t *testing.T) {
	s, _ := openTemp(t)
	s.View(5, func(u *User) {
		if u.ID != 5 || u.BaseCurrency != "RUB" || len(u.Accounts) != 0 || len(u.ExpenseCategories) == 0 {
			t.Errorf("unexpected new user %+v", u)
		}
	})
}

func TestOpenCorruptFileFails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data.db")
	if err := os.WriteFile(p, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(p); err == nil {
		s.Close()
		t.Error("expected error")
	}
}

func TestUpdatePersistsAcrossReopen(t *testing.T) {
	s, p := openTemp(t)
	var accID, archivedID int64
	err := s.Update(42, func(u *User) error {
		a := u.AddAccount("Карта", KindBank, "USD", t0)
		accID = a.ID
		b := u.AddAccount("Старый", KindCash, "RUB", t0)
		archivedID = b.ID
		u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 12345, Category: "c", Note: "n"}, t0)
		u.AddTransaction(Transaction{Type: TxTransfer, AccountID: a.ID, Amount: 100, ToAccountID: b.ID, ToAmount: 9000}, t0)
		u.RemoveAccount(b.ID)
		u.BaseCurrency = "EUR"
		u.SetCategories(TxExpense, []string{"x", "y"})
		u.SetCategories(TxIncome, nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(7, func(u *User) error { u.BaseCurrency = "GBP"; return nil }); err != nil {
		t.Fatal(err)
	}

	s2 := reopen(t, s, p)
	s2.View(42, func(u *User) {
		if u.BaseCurrency != "EUR" {
			t.Errorf("base = %q", u.BaseCurrency)
		}
		if len(u.Accounts) != 2 || u.Accounts[0].Name != "Карта" || u.Accounts[0].ID != accID || !u.Accounts[0].CreatedAt.Equal(t0) {
			t.Errorf("accounts = %+v", u.Accounts)
		}
		if a := u.Account(archivedID); a == nil || !a.Archived || a.Kind != KindCash {
			t.Errorf("archived account = %+v", a)
		}
		if u.DefaultAccountID != accID {
			t.Errorf("default = %d", u.DefaultAccountID)
		}
		if strings.Join(u.ExpenseCategories, ",") != "x,y" || len(u.IncomeCategories) != 0 {
			t.Errorf("categories = %v / %v", u.ExpenseCategories, u.IncomeCategories)
		}
		if len(u.Transactions) != 2 {
			t.Fatalf("transactions = %+v", u.Transactions)
		}
		tx := u.Transactions[0]
		if tx.Type != TxIncome || tx.Amount != 12345 || tx.Category != "c" || tx.Note != "n" || !tx.CreatedAt.Equal(t0) {
			t.Errorf("tx = %+v", tx)
		}
		if tr := u.Transactions[1]; tr.ToAccountID != archivedID || tr.ToAmount != 9000 {
			t.Errorf("transfer = %+v", tr)
		}
		if u.Balance(accID) != 12245 {
			t.Errorf("balance = %d", u.Balance(accID))
		}
		if id := u.nextID(); id <= u.Transactions[1].ID {
			t.Errorf("nextID %d not after %d", id, u.Transactions[1].ID)
		}
	})
	s2.View(7, func(u *User) {
		if u.BaseCurrency != "GBP" {
			t.Errorf("user 7 base = %q", u.BaseCurrency)
		}
	})
}

func TestDeletedTransactionStaysDeletedAfterReopen(t *testing.T) {
	s, p := openTemp(t)
	var keep, drop int64
	_ = s.Update(1, func(u *User) error {
		a := u.AddAccount("a", KindBank, "RUB", t0)
		keep = u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 1}, t0).ID
		drop = u.AddTransaction(Transaction{Type: TxIncome, AccountID: a.ID, Amount: 2}, t0).ID
		return nil
	})
	if err := s.Update(1, func(u *User) error { u.DeleteTransaction(drop); return nil }); err != nil {
		t.Fatal(err)
	}
	reopen(t, s, p).View(1, func(u *User) {
		if len(u.Transactions) != 1 || u.Transactions[0].ID != keep {
			t.Errorf("transactions = %+v", u.Transactions)
		}
	})
}

func TestUpdateErrorDoesNotPersist(t *testing.T) {
	s, p := openTemp(t)
	if err := s.Update(1, func(u *User) error { u.AddAccount("keep", KindBank, "RUB", t0); return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("boom")
	err := s.Update(1, func(u *User) error {
		u.BaseCurrency = "USD"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	reopen(t, s, p).View(1, func(u *User) {
		if u.BaseCurrency != "RUB" || len(u.Accounts) != 1 {
			t.Errorf("persisted user = %+v", u)
		}
	})
}

func TestUpdateErrorOnNewUserDoesNotCreateRow(t *testing.T) {
	s, p := openTemp(t)
	_ = s.Update(1, func(u *User) error { return errors.New("x") })
	s2 := reopen(t, s, p)
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestUpdateWriteFailureRollsBackMemory(t *testing.T) {
	s, p := openTemp(t)
	var accID int64
	_ = s.Update(1, func(u *User) error {
		accID = u.AddAccount("a", KindBank, "RUB", t0).ID
		u.AddTransaction(Transaction{Type: TxIncome, AccountID: accID, Amount: 500}, t0)
		return nil
	})
	if _, err := s.db.Exec(`CREATE TRIGGER fail BEFORE INSERT ON transactions BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.Update(1, func(u *User) error {
		u.BaseCurrency = "USD"
		u.AddTransaction(Transaction{Type: TxExpense, AccountID: accID, Amount: 100}, t0)
		return nil
	})
	if err == nil {
		t.Fatal("expected write error")
	}
	s.View(1, func(u *User) {
		if u.BaseCurrency != "RUB" || len(u.Transactions) != 1 || u.Balance(accID) != 500 {
			t.Errorf("memory not rolled back: base=%s txs=%d balance=%d", u.BaseCurrency, len(u.Transactions), u.Balance(accID))
		}
	})
	reopen(t, s, p).View(1, func(u *User) {
		if u.BaseCurrency != "RUB" || len(u.Transactions) != 1 {
			t.Errorf("partial write persisted: %+v", u)
		}
	})
}

func TestImportJSON(t *testing.T) {
	s, p := openTemp(t)
	jsonPath := filepath.Join(t.TempDir(), "old.json")
	old := `{"version":1,"users":{"9":{"id":9,"base_currency":"USD","default_account_id":1,
		"expense_categories":["Еда"],"income_categories":["ЗП"],
		"accounts":[{"id":1,"name":"Карта","kind":"bank","currency":"USD","created_at":"2025-01-02T03:04:05Z"}],
		"transactions":[{"id":2,"type":"adjust","account_id":1,"amount":1000,"created_at":"2025-01-02T03:04:05Z"},
		                {"id":3,"type":"expense","account_id":1,"amount":250,"category":"Еда","created_at":"2025-01-02T03:04:05Z"}],
		"next_id":4}}}`
	if err := os.WriteFile(jsonPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := s.ImportJSON(jsonPath)
	if err != nil || n != 1 {
		t.Fatalf("import = %d, %v", n, err)
	}
	// A repeated import into a non-empty database does nothing.
	if n, err := s.ImportJSON(jsonPath); err != nil || n != 0 {
		t.Fatalf("second import = %d, %v", n, err)
	}
	if n, err := s.ImportJSON(filepath.Join(t.TempDir(), "missing.json")); err != nil || n != 0 {
		t.Fatalf("missing file import = %d, %v", n, err)
	}
	reopen(t, s, p).View(9, func(u *User) {
		if u.BaseCurrency != "USD" || u.Balance(1) != 750 || u.NextID != 4 || u.ExpenseCategories[0] != "Еда" {
			t.Errorf("imported user = %+v", u)
		}
	})
}

func TestUpdateConcurrent(t *testing.T) {
	s, _ := openTemp(t)
	const n = 20
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = s.Update(1, func(u *User) error {
				u.AddTransaction(Transaction{Type: TxIncome, Amount: 1}, t0)
				return nil
			})
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	s.View(1, func(u *User) {
		if len(u.Transactions) != n {
			t.Errorf("transactions = %d, want %d", len(u.Transactions), n)
		}
		seen := map[int64]bool{}
		for _, tx := range u.Transactions {
			if seen[tx.ID] {
				t.Errorf("duplicate id %d", tx.ID)
			}
			seen[tx.ID] = true
		}
	})
}

func TestUpdateErrorRestoresMemory(t *testing.T) {
	s, _ := openTemp(t)
	_ = s.Update(1, func(u *User) error { u.AddAccount("a", KindBank, "RUB", t0); return nil })
	_ = s.Update(1, func(u *User) error {
		u.BaseCurrency = "USD"
		u.AddAccount("b", KindBank, "USD", t0)
		return errors.New("validation failed late")
	})
	s.View(1, func(u *User) {
		if u.BaseCurrency != "RUB" || len(u.Accounts) != 1 {
			t.Errorf("memory diverged from DB: %+v", u)
		}
	})
}

func TestImportJSONNormalizesNextIDAndCategories(t *testing.T) {
	s, _ := openTemp(t)
	jsonPath := filepath.Join(t.TempDir(), "old.json")
	old := `{"users":{"3":{"accounts":[{"id":5,"name":"a","kind":"cash","currency":"RUB","created_at":"2025-01-02T03:04:05Z"}],
		"transactions":[{"id":7,"type":"income","account_id":5,"amount":100,"created_at":"2025-01-02T03:04:05Z"}]}}}`
	if err := os.WriteFile(jsonPath, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ImportJSON(jsonPath); err != nil || n != 1 {
		t.Fatalf("import = %d, %v", n, err)
	}
	if err := s.Update(3, func(u *User) error {
		u.AddTransaction(Transaction{Type: TxIncome, AccountID: 5, Amount: 1}, t0)
		return nil
	}); err != nil {
		t.Fatalf("add after import: %v", err)
	}
	s.View(3, func(u *User) {
		if u.BaseCurrency != "RUB" || len(u.ExpenseCategories) == 0 || u.Transactions[1].ID <= 7 {
			t.Errorf("not normalized: %+v", u)
		}
	})
}

func TestOpenRejectsNewerSchemaAndBadPath(t *testing.T) {
	s, p := openTemp(t)
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(p); err == nil {
		s2.Close()
		t.Error("expected error for newer schema")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "a?b.db")); err == nil {
		t.Error("expected error for path with '?'")
	}
}
