// Package storage хранит данные пользователей в SQLite.
//
// Все пользователи загружаются в память при открытии (бот личный, данных немного),
// поэтому чтение не обращается к базе. Каждое изменение записывается в одной
// SQL-транзакции: настройки, категории и счета перезаписываются целиком
// (их единицы), а операции — только добавленные и удалённые.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // драйвер SQLite на чистом Go, без CGO
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id                 INTEGER PRIMARY KEY,
	base_currency      TEXT    NOT NULL,
	default_account_id INTEGER NOT NULL DEFAULT 0,
	next_id            INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS categories (
	user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	type     TEXT    NOT NULL,
	position INTEGER NOT NULL,
	name     TEXT    NOT NULL,
	PRIMARY KEY (user_id, type, position)
);
CREATE TABLE IF NOT EXISTS accounts (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id         INTEGER NOT NULL,
	name       TEXT    NOT NULL,
	kind       TEXT    NOT NULL,
	currency   TEXT    NOT NULL,
	archived   INTEGER NOT NULL DEFAULT 0,
	created_at TEXT    NOT NULL,
	PRIMARY KEY (user_id, id)
);
CREATE TABLE IF NOT EXISTS transactions (
	user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id            INTEGER NOT NULL,
	type          TEXT    NOT NULL,
	account_id    INTEGER NOT NULL,
	amount        INTEGER NOT NULL,
	to_account_id INTEGER NOT NULL DEFAULT 0,
	to_amount     INTEGER NOT NULL DEFAULT 0,
	category      TEXT    NOT NULL DEFAULT '',
	note          TEXT    NOT NULL DEFAULT '',
	created_at    TEXT    NOT NULL,
	PRIMARY KEY (user_id, id)
);
CREATE INDEX IF NOT EXISTS transactions_by_time ON transactions (user_id, created_at);
`

// schemaVersion — текущая версия схемы (PRAGMA user_version). Будущие миграции
// добавляются в migrate и применяются по номеру версии.
const schemaVersion = 1

// timeLayout фиксированной ширины, чтобы строки сортировались так же, как время.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

type Store struct {
	mu    sync.Mutex
	db    *sql.DB
	users map[int64]*User
}

func Open(path string) (*Store, error) {
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("путь к базе не должен содержать символы ?, # или %%: %s", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("создание каталога данных: %w", err)
	}
	if dir != "." {
		_ = os.Chmod(dir, 0o700) // MkdirAll не меняет права существующего каталога
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открытие базы: %w", err)
	}
	// Одно соединение: запись и так идёт последовательно, а pragma применяются к нему.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("инициализация базы %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)

	users, err := loadUsers(context.Background(), db, 0)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, users: users}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("база создана более новой версией бота (схема %d, поддерживается %d)", version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	_, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) user(id int64) *User {
	u, ok := s.users[id]
	if !ok {
		u = newUser(id)
		s.users[id] = u
	}
	return u
}

// View даёт доступ к данным пользователя только для чтения.
// Указатели из fn нельзя использовать после её завершения.
func (s *Store) View(id int64, fn func(u *User)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.user(id))
}

// Update изменяет данные пользователя и сохраняет их. Если fn вернула ошибку или
// запись в базу не удалась, состояние в памяти перечитывается из базы.
func (s *Store) Update(id int64, fn func(u *User) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.user(id)
	before := make(map[int64]bool, len(u.Transactions))
	for _, t := range u.Transactions {
		before[t.ID] = true
	}
	ctx := context.Background()
	if err := fn(u); err != nil {
		s.reload(ctx, id)
		return err
	}
	if err := s.inTx(ctx, func(tx *sql.Tx) error { return persist(ctx, tx, u, before) }); err != nil {
		s.reload(ctx, id)
		return fmt.Errorf("сохранение: %w", err)
	}
	return nil
}

func (s *Store) reload(ctx context.Context, id int64) {
	users, err := loadUsers(ctx, s.db, id)
	if err != nil {
		slog.Error("не удалось перечитать пользователя из базы", "user", id, "err", err)
		return
	}
	if u, ok := users[id]; ok {
		s.users[id] = u
	} else {
		delete(s.users, id)
	}
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // no-op после Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// persist записывает пользователя. before — ID операций, которые уже есть в базе.
func persist(ctx context.Context, tx *sql.Tx, u *User, before map[int64]bool) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, base_currency, default_account_id, next_id) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			base_currency = excluded.base_currency,
			default_account_id = excluded.default_account_id,
			next_id = excluded.next_id`,
		u.ID, u.BaseCurrency, u.DefaultAccountID, u.NextID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	for _, typ := range []TxType{TxExpense, TxIncome} {
		for i, name := range u.Categories(typ) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO categories (user_id, type, position, name) VALUES (?, ?, ?, ?)`,
				u.ID, string(typ), i, name); err != nil {
				return err
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	for _, a := range u.Accounts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO accounts (user_id, id, name, kind, currency, archived, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.ID, a.ID, a.Name, string(a.Kind), a.Currency, a.Archived, a.CreatedAt.UTC().Format(timeLayout)); err != nil {
			return err
		}
	}

	// Операции не редактируются, поэтому достаточно вставить новые и удалить исчезнувшие.
	insert, err := tx.PrepareContext(ctx, `
		INSERT INTO transactions (user_id, id, type, account_id, amount, to_account_id, to_amount, category, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	current := make(map[int64]bool, len(u.Transactions))
	for _, t := range u.Transactions {
		current[t.ID] = true
		if before[t.ID] {
			continue
		}
		if _, err := insert.ExecContext(ctx, u.ID, t.ID, string(t.Type), t.AccountID, t.Amount,
			t.ToAccountID, t.ToAmount, t.Category, t.Note, t.CreatedAt.UTC().Format(timeLayout)); err != nil {
			return err
		}
	}
	for id := range before {
		if !current[id] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM transactions WHERE user_id = ? AND id = ?`, u.ID, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadUsers читает пользователей из базы; onlyID == 0 — всех.
func loadUsers(ctx context.Context, db *sql.DB, onlyID int64) (map[int64]*User, error) {
	users := map[int64]*User{}

	err := query(ctx, db, `SELECT id, base_currency, default_account_id, next_id FROM users WHERE (? = 0 OR id = ?)`,
		onlyID, func(rows *sql.Rows) error {
			u := &User{}
			if err := rows.Scan(&u.ID, &u.BaseCurrency, &u.DefaultAccountID, &u.NextID); err != nil {
				return err
			}
			users[u.ID] = u
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("чтение пользователей: %w", err)
	}

	err = query(ctx, db, `SELECT user_id, type, name FROM categories WHERE (? = 0 OR user_id = ?) ORDER BY user_id, type, position`,
		onlyID, func(rows *sql.Rows) error {
			var uid int64
			var typ, name string
			if err := rows.Scan(&uid, &typ, &name); err != nil {
				return err
			}
			if u := users[uid]; u != nil {
				t := TxType(typ)
				u.SetCategories(t, append(u.Categories(t), name))
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("чтение категорий: %w", err)
	}

	err = query(ctx, db, `SELECT user_id, id, name, kind, currency, archived, created_at FROM accounts WHERE (? = 0 OR user_id = ?) ORDER BY user_id, id`,
		onlyID, func(rows *sql.Rows) error {
			var uid int64
			var created string
			a := &Account{}
			if err := rows.Scan(&uid, &a.ID, &a.Name, &a.Kind, &a.Currency, &a.Archived, &created); err != nil {
				return err
			}
			var err error
			if a.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
				return err
			}
			if u := users[uid]; u != nil {
				u.Accounts = append(u.Accounts, a)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("чтение счетов: %w", err)
	}

	err = query(ctx, db, `
		SELECT user_id, id, type, account_id, amount, to_account_id, to_amount, category, note, created_at
		FROM transactions WHERE (? = 0 OR user_id = ?) ORDER BY user_id, id`,
		onlyID, func(rows *sql.Rows) error {
			var uid int64
			var created string
			t := &Transaction{}
			if err := rows.Scan(&uid, &t.ID, &t.Type, &t.AccountID, &t.Amount, &t.ToAccountID, &t.ToAmount,
				&t.Category, &t.Note, &created); err != nil {
				return err
			}
			var err error
			if t.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
				return err
			}
			if u := users[uid]; u != nil {
				u.Transactions = append(u.Transactions, t)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("чтение операций: %w", err)
	}
	return users, nil
}

func query(ctx context.Context, db *sql.DB, q string, id int64, scan func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q, id, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
