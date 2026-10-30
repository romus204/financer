package storage

import (
	"slices"
	"time"
)

type AccountKind string

const (
	KindBank AccountKind = "bank" // card or bank account
	KindCash AccountKind = "cash" // cash
)

type TxType string

const (
	TxExpense  TxType = "expense"
	TxIncome   TxType = "income"
	TxTransfer TxType = "transfer"
	TxAdjust   TxType = "adjust" // opening balance or adjustment; Amount is signed
)

type Account struct {
	ID        int64       `json:"id"`
	Name      string      `json:"name"`
	Kind      AccountKind `json:"kind"`
	Currency  string      `json:"currency"`
	Archived  bool        `json:"archived,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

// Transaction is a single operation. Amounts are in hundredths of the account's currency.
// For a transfer, Amount is debited from AccountID and ToAmount is credited to ToAccountID.
type Transaction struct {
	ID          int64     `json:"id"`
	Type        TxType    `json:"type"`
	AccountID   int64     `json:"account_id"`
	Amount      int64     `json:"amount"`
	ToAccountID int64     `json:"to_account_id,omitempty"`
	ToAmount    int64     `json:"to_amount,omitempty"`
	Category    string    `json:"category,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// User holds all data of a single user. Balances are not stored but computed from
// transactions, so deleting any transaction automatically corrects the balances.
type User struct {
	ID                int64          `json:"id"`
	BaseCurrency      string         `json:"base_currency"`
	DefaultAccountID  int64          `json:"default_account_id,omitempty"`
	ExpenseCategories []string       `json:"expense_categories"`
	IncomeCategories  []string       `json:"income_categories"`
	Accounts          []*Account     `json:"accounts"`
	Transactions      []*Transaction `json:"transactions"`
	NextID            int64          `json:"next_id"`
}

var (
	defaultExpenseCategories = []string{
		"🛒 Продукты", "🍽 Кафе", "🚕 Транспорт", "🏠 Жильё", "💡 Коммуналка",
		"📱 Связь", "👕 Одежда", "💊 Здоровье", "🎉 Развлечения", "🎁 Подарки", "📦 Другое",
	}
	defaultIncomeCategories = []string{
		"💼 Зарплата", "💻 Подработка", "📈 Проценты", "🎁 Подарок", "📦 Другое",
	}
)

func newUser(id int64) *User {
	return &User{
		ID:                id,
		BaseCurrency:      "RUB",
		ExpenseCategories: slices.Clone(defaultExpenseCategories),
		IncomeCategories:  slices.Clone(defaultIncomeCategories),
		NextID:            1,
	}
}

func (u *User) nextID() int64 {
	if u.NextID < 1 {
		u.NextID = 1
	}
	id := u.NextID
	u.NextID++
	return id
}

// Categories returns the expense or income categories.
func (u *User) Categories(t TxType) []string {
	if t == TxIncome {
		return u.IncomeCategories
	}
	return u.ExpenseCategories
}

func (u *User) SetCategories(t TxType, cats []string) {
	if t == TxIncome {
		u.IncomeCategories = cats
	} else {
		u.ExpenseCategories = cats
	}
}

// Account looks up an account, including deleted (archived) ones.
func (u *User) Account(id int64) *Account {
	for _, a := range u.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (u *User) ActiveAccount(id int64) *Account {
	if a := u.Account(id); a != nil && !a.Archived {
		return a
	}
	return nil
}

func (u *User) ActiveAccounts() []*Account {
	var res []*Account
	for _, a := range u.Accounts {
		if !a.Archived {
			res = append(res, a)
		}
	}
	return res
}

// DefaultAccount returns the main account for quick entries.
func (u *User) DefaultAccount() *Account {
	if a := u.ActiveAccount(u.DefaultAccountID); a != nil {
		return a
	}
	if accs := u.ActiveAccounts(); len(accs) > 0 {
		return accs[0]
	}
	return nil
}

// AccountByCurrency picks an account in the given currency, preferring the main one.
func (u *User) AccountByCurrency(cur string) *Account {
	if d := u.DefaultAccount(); d != nil && d.Currency == cur {
		return d
	}
	for _, a := range u.ActiveAccounts() {
		if a.Currency == cur {
			return a
		}
	}
	return nil
}

func (u *User) AddAccount(name string, kind AccountKind, currency string, now time.Time) *Account {
	a := &Account{ID: u.nextID(), Name: name, Kind: kind, Currency: currency, CreatedAt: now}
	u.Accounts = append(u.Accounts, a)
	if u.ActiveAccount(u.DefaultAccountID) == nil {
		u.DefaultAccountID = a.ID
	}
	return a
}

// RemoveAccount deletes an account. If it has transactions, the account is archived
// instead so that the history stays readable.
func (u *User) RemoveAccount(id int64) (archived bool) {
	if u.HasTransactions(id) {
		if a := u.Account(id); a != nil {
			a.Archived = true
		}
		archived = true
	} else {
		u.Accounts = slices.DeleteFunc(u.Accounts, func(a *Account) bool { return a.ID == id })
	}
	if u.DefaultAccountID == id {
		u.DefaultAccountID = 0
		if d := u.DefaultAccount(); d != nil {
			u.DefaultAccountID = d.ID
		}
	}
	return archived
}

func (u *User) HasTransactions(accID int64) bool {
	return slices.ContainsFunc(u.Transactions, func(t *Transaction) bool {
		return t.AccountID == accID || t.ToAccountID == accID
	})
}

// Balances computes the balances of all accounts from transactions.
func (u *User) Balances() map[int64]int64 {
	b := make(map[int64]int64, len(u.Accounts))
	for _, t := range u.Transactions {
		switch t.Type {
		case TxExpense:
			b[t.AccountID] -= t.Amount
		case TxIncome, TxAdjust:
			b[t.AccountID] += t.Amount
		case TxTransfer:
			b[t.AccountID] -= t.Amount
			b[t.ToAccountID] += t.ToAmount
		}
	}
	return b
}

func (u *User) Balance(accID int64) int64 { return u.Balances()[accID] }

func (u *User) AddTransaction(t Transaction, now time.Time) *Transaction {
	t.ID = u.nextID()
	t.CreatedAt = now
	u.Transactions = append(u.Transactions, &t)
	return &t
}

func (u *User) Transaction(id int64) *Transaction {
	for _, t := range u.Transactions {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (u *User) DeleteTransaction(id int64) bool {
	i := slices.IndexFunc(u.Transactions, func(t *Transaction) bool { return t.ID == id })
	if i < 0 {
		return false
	}
	u.Transactions = slices.Delete(u.Transactions, i, i+1)
	return true
}
