package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
)

// ImportJSON migrates data from the legacy JSON storage. The import runs in a single
// transaction and only into an empty database; it returns the number of users migrated.
func (s *Store) ImportJSON(path string) (int, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var f struct {
		Users map[int64]*User `json:"users"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return 0, fmt.Errorf("разбор %s: %w", path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ctx := context.Background()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}

	imported := map[int64]*User{}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		for id, u := range f.Users {
			if u == nil {
				continue
			}
			u.ID = id
			normalize(u)
			if err := persist(ctx, tx, u, nil); err != nil {
				return fmt.Errorf("пользователь %d: %w", id, err)
			}
			imported[id] = u
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for id, u := range imported {
		s.users[id] = u
	}
	return len(imported), nil
}

// normalize fixes legacy-format data: NextID must be greater than all IDs,
// and missing categories are replaced with the defaults.
func normalize(u *User) {
	if u.BaseCurrency == "" {
		u.BaseCurrency = "RUB"
	}
	if u.ExpenseCategories == nil {
		u.ExpenseCategories = slices.Clone(defaultExpenseCategories)
	}
	if u.IncomeCategories == nil {
		u.IncomeCategories = slices.Clone(defaultIncomeCategories)
	}
	u.Accounts = slices.DeleteFunc(u.Accounts, func(a *Account) bool { return a == nil })
	u.Transactions = slices.DeleteFunc(u.Transactions, func(t *Transaction) bool { return t == nil })
	for _, a := range u.Accounts {
		u.NextID = max(u.NextID, a.ID+1)
	}
	for _, t := range u.Transactions {
		u.NextID = max(u.NextID, t.ID+1)
	}
	u.NextID = max(u.NextID, 1)
}
