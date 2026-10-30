# Financer — personal finance Telegram bot

**English** | [Русский](README.ru.md)

A personal bot for tracking money on cards, bank accounts and in cash — in any currency.
Written in Go; the only external dependency is the SQLite driver [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) (pure Go, no CGO).

> The bot's interface is in Russian.

## Features

- **Two account types**: 💳 card / bank account and 💵 cash, each in its own currency (RUB, USD, EUR, KZT, … or any code, e.g. `USDT`).
- **Quick entry in a single message**:
  - `350 кофе` — an expense from the main account;
  - `+50000 зарплата` — income;
  - `20$ такси`, `15 eur обед`, `5000 тг` — the account is picked by currency.

  After the input the bot shows category buttons; on the same screen you can switch the account and add a note.
- **Transfers** between your own accounts: cash withdrawals, card top-ups, currency exchange. If the currencies differ, the bot asks how much was received and suggests the amount at the current rate.
- **Balance**: per-account balances, totals per currency and a grand total in the main currency at the current rate.
- **History** with pagination, overall or per account; any transaction can be opened and deleted.
- **Monthly report**: income, expenses and a per-category breakdown with percentages.
- **Balance adjustment**: enter the actual balance and the difference is recorded as a separate transaction.
- **↩️ Undo** right after every entry.
- **Settings**: main currency, custom expense and income categories, CSV export (opens in Excel).

## Getting started

1. Create a bot with [@BotFather](https://t.me/BotFather) and get a token.
2. Copy `.env.example` to `.env` and fill it in:

   ```env
   TELEGRAM_TOKEN=123456:ABC...
   ALLOWED_USER_IDS=123456789
   TIMEZONE=Europe/Moscow
   ```

   To find your Telegram ID, message the bot without access: it will reply with your ID.
3. Build and run:

   ```sh
   go mod tidy        # fetches the SQLite driver
   go build -o financer .
   ./financer
   ```

### Docker

```sh
go mod tidy                     # once: go.sum is required to build the image
docker compose up -d --build
docker compose logs -f
```

The image builds a static binary on top of `distroless/static` and runs as non-root. Data lives in the `financer-data` volume (`/data/financer.db`).
Backups: `docker compose exec financer` won't work (the image has no shell), so copy the volume file with the container stopped, or run `sqlite3 financer.db ".backup backup.db"` on the host.

### systemd server

```sh
./deploy/deploy.sh                 # host is taken from $DEPLOY_HOST or the first argument
./deploy/deploy.sh myserver --env  # another host; --env re-uploads the local .env
```

The script works both on a fresh server and for updates. It:
- creates the `financer` user;
- creates the `/opt/financer` and `/opt/financer/data` directories;
- uploads `.env` on the first deploy;
- backs up the database before updating (the last 10 backups are kept);
- installs the systemd unit and checks that the service has started.

Requires root or passwordless `sudo` on the server.

Environment variables take precedence over `.env`.

| Variable           | Default                                 | Description                                  |
|--------------------|-----------------------------------------|----------------------------------------------|
| `TELEGRAM_TOKEN`   | —                                       | Bot token (required)                         |
| `ALLOWED_USER_IDS` | —                                       | Comma-separated IDs of allowed users         |
| `DATA_FILE`        | `data/financer.db`                      | SQLite database                              |
| `LEGACY_JSON`      | `<DATA_FILE dir>/financer.json`         | Legacy JSON file for a one-time import       |
| `TIMEZONE`         | system                                  | Time zone for dates and reports              |
| `RATES_URL`        | `https://open.er-api.com/v6/latest/USD` | Exchange rate source; `off` disables conversion |

## Architecture

```
main.go               configuration and startup
internal/tg           minimal Telegram Bot API client (long polling)
internal/bot          dialogs, menus, screens
internal/storage      data model and SQLite storage
internal/money        parsing and formatting of amounts and currencies
internal/rates        cached exchange rates
```

- Amounts are stored as integers in hundredths, no floats.
- Balances are not stored but computed from transactions, so deleting or undoing a transaction always yields a correct balance.
- Data is stored in SQLite (WAL mode). Every change is written in a single transaction, and if a write fails the in-memory state is reloaded from the database. The database file is created with `0600` permissions and its directory with `0700`.
- If a `financer.json` from the legacy version sits next to the database, its data is migrated into the empty database on first start and the file is renamed to `financer.json.imported`.
- The bot works only in private chats and only for users listed in `ALLOWED_USER_IDS`. Each user has their own data.
- Exchange rates are used only for converting totals and for the exchange hint. Only the rate list request goes to the external service — none of your data.
