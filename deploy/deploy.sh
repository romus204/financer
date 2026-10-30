#!/usr/bin/env bash
# Развёртывание financer на сервер с systemd — с нуля или обновление.
#
#   ./deploy/deploy.sh [host] [--env]
#
#   host   SSH-хост (по умолчанию $DEPLOY_HOST; один из них обязателен)
#   --env  загрузить локальный .env, даже если на сервере он уже есть
#
# Что делает (каждый шаг идемпотентен):
#   1. собирает статический бинарник под архитектуру сервера;
#   2. создаёт системного пользователя и каталоги, если их нет;
#   3. при первом развёртывании (или с --env) кладёт .env на сервер;
#   4. останавливает сервис, делает резервную копию базы, ставит новый бинарник;
#   5. устанавливает unit systemd, включает автозапуск и проверяет, что сервис поднялся.
#
# На сервере нужен root или sudo без пароля.
set -euo pipefail

APP=financer
APP_DIR=/opt/$APP
DATA_DIR=$APP_DIR/data
BACKUPS_KEEP=10

HOST=${DEPLOY_HOST:-}
FORCE_ENV=0
for arg in "$@"; do
	case $arg in
	--env) FORCE_ENV=1 ;;
	-h | --help)
		sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*)
		echo "Неизвестный флаг: $arg" >&2
		exit 2
		;;
	*) HOST=$arg ;;
	esac
done

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DEPLOY_DIR=$ROOT/deploy
log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() {
	printf '\033[1;31mОшибка:\033[0m %s\n' "$*" >&2
	exit 1
}

[[ -n $HOST ]] || die "не задан хост: передай его аргументом или через DEPLOY_HOST"

# --- проверки и сбор сведений о сервере ---

log "Подключаюсь к $HOST"
read -r REMOTE_ARCH PRIV HAS_ENV < <(ssh "$HOST" "
	case \$(uname -m) in
		x86_64|amd64) arch=amd64 ;;
		aarch64|arm64) arch=arm64 ;;
		armv7l) arch=arm ;;
		*) arch=unknown ;;
	esac
	if [ \"\$(id -u)\" -eq 0 ]; then priv=root
	elif sudo -n true 2>/dev/null; then priv=sudo
	else priv=none; fi
	if [ -f $APP_DIR/.env ] || sudo -n test -f $APP_DIR/.env 2>/dev/null; then env=yes; else env=no; fi
	command -v systemctl >/dev/null || arch=nosystemd
	echo \$arch \$priv \$env
") || die "не удалось подключиться к $HOST по SSH"

[[ $REMOTE_ARCH == nosystemd ]] && die "на сервере нет systemd"
[[ $REMOTE_ARCH == unknown ]] && die "неподдерживаемая архитектура сервера"
case $PRIV in
root) SUDO="" ;;
sudo) SUDO="sudo" ;;
*) die "нужен root или sudo без пароля (NOPASSWD) на $HOST" ;;
esac

UPLOAD_ENV=0
if [[ $HAS_ENV == no || $FORCE_ENV == 1 ]]; then
	[[ -f $ROOT/.env ]] || die "на сервере нет .env, а локального $ROOT/.env тоже нет — создайте его из .env.example"
	grep -Eq '^[[:space:]]*TELEGRAM_TOKEN=.+' "$ROOT/.env" || die "в .env не задан TELEGRAM_TOKEN"
	grep -Eq '^[[:space:]]*ALLOWED_USER_IDS=.+' "$ROOT/.env" || echo "Предупреждение: ALLOWED_USER_IDS пуст — бот никого не пустит" >&2
	UPLOAD_ENV=1
fi

# --- сборка ---

BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT

log "Тесты"
(cd "$ROOT" && go test ./...) >/dev/null || die "тесты не прошли, развёртывание отменено (подробности: go test ./...)"

log "Сборка linux/$REMOTE_ARCH"
(cd "$ROOT" && GOOS=linux GOARCH=$REMOTE_ARCH CGO_ENABLED=0 \
	go build -trimpath -ldflags="-s -w" -o "$BUILD_DIR/$APP" .)

# Установочная часть выполняется на сервере от root. Она загружается файлом, а не
# через stdin, чтобы команды внутри (apt-get и т. п.) не прочитали остаток скрипта.
cat >"$BUILD_DIR/install.sh" <<'REMOTE'
#!/usr/bin/env bash
set -euo pipefail
APP=$1 APP_DIR=$2 DATA_DIR=$3 TMP=$4 UPLOAD_ENV=$5 KEEP=$6
trap 'rm -rf "$TMP"' EXIT
step() { printf '    - %s\n' "$*"; }

# Корневые сертификаты нужны для HTTPS к Telegram и сервису курсов.
if [ ! -s /etc/ssl/certs/ca-certificates.crt ] && [ ! -s /etc/pki/tls/certs/ca-bundle.crt ]; then
	step "устанавливаю ca-certificates"
	if command -v apt-get >/dev/null; then apt-get update -qq && apt-get install -y -qq ca-certificates >/dev/null
	elif command -v dnf >/dev/null; then dnf install -y -q ca-certificates
	elif command -v yum >/dev/null; then yum install -y -q ca-certificates
	elif command -v apk >/dev/null; then apk add -q ca-certificates
	else echo "Не удалось установить ca-certificates: неизвестный пакетный менеджер" >&2; exit 1; fi
fi

if ! getent group "$APP" >/dev/null; then
	step "создаю группу $APP"
	groupadd --system "$APP"
fi
if ! id "$APP" >/dev/null 2>&1; then
	step "создаю пользователя $APP"
	NOLOGIN=$(command -v nologin || echo /usr/sbin/nologin)
	useradd --system --gid "$APP" --home-dir "$APP_DIR" --no-create-home --shell "$NOLOGIN" "$APP"
fi

# Бинарник и .env принадлежат root: сервис может их читать, но не изменить.
# Писать он может только в каталог данных.
install -d -m 0750 -o root -g "$APP" "$APP_DIR"
install -d -m 0700 -o "$APP" -g "$APP" "$DATA_DIR" "$DATA_DIR/backups"

if [ "$UPLOAD_ENV" = 1 ]; then
	step "устанавливаю .env"
	install -m 0640 -o root -g "$APP" "$TMP/.env" "$APP_DIR/.env"
fi

if systemctl is-active --quiet "$APP"; then
	step "останавливаю сервис"
	systemctl stop "$APP"
fi

# Резервная копия базы (сервис остановлен, поэтому копия согласованна).
if [ -f "$DATA_DIR/$APP.db" ]; then
	TS=$(date +%Y%m%d-%H%M%S)
	step "резервная копия базы: backups/$APP-$TS.db"
	for f in "$DATA_DIR/$APP.db" "$DATA_DIR/$APP.db-wal"; do
		[ -f "$f" ] && install -m 0600 -o "$APP" -g "$APP" "$f" "$DATA_DIR/backups/$(basename "$f" | sed "s/^$APP/$APP-$TS/")"
	done
	ls -1t "$DATA_DIR/backups/$APP"-*.db 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
		rm -f "$old" "$old-wal"
	done
fi

step "устанавливаю бинарник"
install -m 0755 -o root -g root "$TMP/$APP" "$APP_DIR/$APP.new"
mv -f "$APP_DIR/$APP.new" "$APP_DIR/$APP"

step "устанавливаю unit systemd"
install -m 0644 -o root -g root "$TMP/$APP.service" "/etc/systemd/system/$APP.service"
systemctl daemon-reload
systemctl enable --quiet "$APP"
systemctl restart "$APP"

sleep 3
if systemctl is-active --quiet "$APP"; then
	step "сервис запущен"
	journalctl -u "$APP" -n 5 --no-pager -o cat || true
else
	echo "Сервис не запустился. Последние строки журнала:" >&2
	journalctl -u "$APP" -n 30 --no-pager -o cat >&2 || true
	exit 1
fi
REMOTE

# --- загрузка ---

log "Загрузка файлов"
REMOTE_TMP=$(ssh "$HOST" "mktemp -d")
FILES=("$BUILD_DIR/$APP" "$BUILD_DIR/install.sh" "$DEPLOY_DIR/$APP.service")
((UPLOAD_ENV)) && FILES+=("$ROOT/.env")
scp -q "${FILES[@]}" "$HOST:$REMOTE_TMP/"

# --- установка на сервере ---

log "Установка на сервере"
ssh "$HOST" "$SUDO bash '$REMOTE_TMP/install.sh' '$APP' '$APP_DIR' '$DATA_DIR' '$REMOTE_TMP' '$UPLOAD_ENV' '$BACKUPS_KEEP'" </dev/null

log "Готово: $APP развёрнут на $HOST"
echo "    Логи:   ssh $HOST journalctl -u $APP -f"
echo "    Статус: ssh $HOST systemctl status $APP"
