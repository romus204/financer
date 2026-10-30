package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // часовые пояса без зависимости от системы

	"financer/internal/bot"
	"financer/internal/rates"
	"financer/internal/storage"
	"financer/internal/tg"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := loadDotEnv(".env"); err != nil {
		slog.Warn("не удалось прочитать .env", "err", err)
	}
	if err := run(); err != nil {
		slog.Error("бот остановлен", "err", err)
		os.Exit(1)
	}
}

func run() error {
	token := strings.TrimSpace(os.Getenv("TELEGRAM_TOKEN"))
	if token == "" {
		return errors.New("не задан TELEGRAM_TOKEN")
	}
	allowed, err := parseIDs(os.Getenv("ALLOWED_USER_IDS"))
	if err != nil {
		return err
	}
	if len(allowed) == 0 {
		slog.Warn("ALLOWED_USER_IDS пуст: бот никого не пустит, а лишь сообщит пользователю его ID")
	}

	loc := time.Local
	if tz := strings.TrimSpace(os.Getenv("TIMEZONE")); tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return fmt.Errorf("TIMEZONE: %w", err)
		}
	}

	ratesURL := rates.DefaultURL
	if v, ok := os.LookupEnv("RATES_URL"); ok {
		ratesURL = strings.TrimSpace(v)
	}
	if ratesURL == "off" {
		ratesURL = ""
	}

	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "data/financer.db"
	}
	store, err := storage.Open(dataFile)
	if err != nil {
		return err
	}
	defer store.Close()

	// Перенос данных из прежней JSON-версии бота (однократно, только в пустую базу).
	legacy := os.Getenv("LEGACY_JSON")
	if legacy == "" {
		legacy = filepath.Join(filepath.Dir(dataFile), "financer.json")
	}
	if n, err := store.ImportJSON(legacy); err != nil {
		return fmt.Errorf("импорт %s: %w", legacy, err)
	} else if n > 0 {
		if err := os.Rename(legacy, legacy+".imported"); err != nil {
			slog.Warn("не удалось переименовать импортированный файл", "file", legacy, "err", err)
		}
		slog.Info("данные перенесены из JSON", "users", n, "file", legacy)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := bot.New(tg.New(token), store, rates.New(ratesURL), bot.Config{AllowedUsers: allowed, Location: loc})
	slog.Info("бот запущен", "data", dataFile, "allowed_users", len(allowed), "tz", loc.String())
	return b.Run(ctx)
}

func parseIDs(s string) (map[int64]bool, error) {
	ids := map[int64]bool{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_USER_IDS: некорректный ID %q", part)
		}
		ids[id] = true
	}
	return ids, nil
}

// loadDotEnv читает KEY=VALUE из файла, не перезаписывая уже заданные переменные окружения.
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}
