# JSOL Uptime Bot

![Go](https://img.shields.io/badge/Go-1.26-blue)
![License](https://img.shields.io/github/license/jsolteam/jsol-uptime-bot)
![CI](https://github.com/jsolteam/jsol-uptime-bot/actions/workflows/publish.yaml/badge.svg)

---

Telegram-бот для мониторинга серверов JSOL Team. Каждый узел независимо проверяет все серверы, получает метрики агента и хранит историю в собственной SQLite-базе. Telegram polling запускает только выбранный ведущий узел.

---

## Возможности

| Функция | Пакет |
|---|---|
| ICMP ping, HTTP-агент и системные метрики | `internal/monitor`, `internal/agent` |
| Проверка интернета и доступности Telegram | `internal/cluster` |
| Выбор Telegram-ведущего по приоритету и большинству доступных узлов | `internal/cluster` |
| SQLite, измерения, события и резервные копии | `internal/storage` |
| Telegram меню, графики, уведомления и ежедневный отчёт | `internal/telegram` |

## Требования

- Docker Compose v2.
- Linux сервер с публичным IP и доменом, указывающим на этот IP.
- Входящие TCP-порты 80 (выпуск/обновление сертификата) и 8080 (агент), ICMP Echo между узлами.
- Certbot для выпуска Let's Encrypt сертификата.
- Одинаковый `AGENT_TOKEN` и токен Telegram-бота на всех экземплярах.

## Запуск

Ниже команды для Ubuntu/Debian. Повторите шаги для каждого сервера, заменив `node-1.example.com` на его домен. DNS A-запись домена должна вести на публичный IP сервера; если есть AAAA-запись, IPv6 также должен быть настроен.

Установите Docker Engine с Compose plugin по [официальной инструкции Docker](https://docs.docker.com/engine/install/), затем установите Certbot. Склонируйте проект, например, в `/opt/jsol-uptime-bot`:

```sh
sudo apt update
sudo apt install -y certbot git
sudo install -d -o "$USER" -g "$(id -gn)" /opt/jsol-uptime-bot
git clone https://github.com/jsolteam/jsol-uptime-bot.git /opt/jsol-uptime-bot
cd /opt/jsol-uptime-bot
```

Откройте TCP 80 снаружи для проверки Let's Encrypt HTTP-01. Порт 80 нужен при выпуске и продлении сертификата. Убедитесь, что другой процесс его не занимает. Выпустите сертификат:

```sh
sudo certbot certonly --standalone --preferred-challenges http \
  --agree-tos --no-eff-email --email admin@example.com \
  -d node-1.example.com
```

Certbot временно поднимет HTTP-сервер на порту 80. Подготовьте deploy hook, который позже подключите для копирования обновлённого сертификата:

```sh
sudo chmod +x /opt/jsol-uptime-bot/deploy/certbot-deploy.sh
```

Продолжайте настройку; сначала подготовьте конфиг и `.env`, затем скопируйте сертификат перед первым запуском.

Скопируйте пример конфигурации и отредактируйте его:

```sh
cd /opt/jsol-uptime-bot
cp configs/config.example.yaml configs/config.yaml
mkdir -p certs
```

В `servers` замените примеры адресов на реальные публичные IP и HTTPS URL агентов (`https://node-1.example.com:8080`). Для каждого сервера создайте конфиг из общего примера, задайте собственные `node.id` и `node.priority`, а список `servers` оставьте одинаковым. В `node.id` укажите ID этого узла из списка. Приоритет узла должен совпадать в обоих местах. Для одиночного локального запуска оставьте в `servers` только этот узел и используйте `http://127.0.0.1:8080`, убрав настройки TLS.

Оставьте `agent.tls_cert_file: /etc/jsol/certs/fullchain.pem` и `agent.tls_key_file: /etc/jsol/certs/privkey.pem`: Compose монтирует `./certs` в `/etc/jsol/certs`. Настройки `monitoring.interval`, `timeout`, `failure_threshold` и `recovery_threshold` управляют частотой и устойчивостью проверок. `monitoring.internet_checks` требует не менее двух HTTPS URL на разных доменах.

Создайте `.env` рядом с `compose.yaml`. Токен Telegram получите у BotFather. `AGENT_TOKEN` должен быть одинаковым на всех узлах и случайным:

```sh
umask 077
cat > .env <<'EOF'
TELEGRAM_BOT_TOKEN=токен_от_BotFather
AGENT_TOKEN=замените_на_вывод_openssl_rand_hex_32
EOF
chmod 600 .env
openssl rand -hex 32
```

Вставьте результат `openssl rand -hex 32` как значение `AGENT_TOKEN`. Файл `.env` исключён из Git. Запустите узел:

```sh
sudo install -o 10001 -g 10001 -m 0444 \
  /etc/letsencrypt/live/node-1.example.com/fullchain.pem certs/fullchain.pem
sudo install -o 10001 -g 10001 -m 0400 \
  /etc/letsencrypt/live/node-1.example.com/privkey.pem certs/privkey.pem
docker compose up -d --build
docker compose ps
docker compose logs -f uptime-bot
```

После первого запуска подключите автоматическое обновление. Hook копирует новые файлы и перезапускает контейнер только после успешного продления:

```sh
sudo install -d /etc/letsencrypt/renewal-hooks/deploy
sudo ln -sf /opt/jsol-uptime-bot/deploy/certbot-deploy.sh \
  /etc/letsencrypt/renewal-hooks/deploy/jsol-uptime-bot
systemctl list-timers | grep certbot
sudo certbot renew --dry-run
```

Убедитесь, что таймер Certbot включён. `--dry-run` проверяет процесс продления; deploy hook вызывается только при реальном успешном продлении.

Откройте TCP 8080 во внешнем firewall только для IP других узлов, если провайдер позволяет задать такие правила. На сервере также разрешите ICMP Echo между узлами. `AGENT_BIND_ADDRESS` и `AGENT_PUBLIC_PORT` задают адрес и порт агента; стандартно используется `0.0.0.0:8080`.

Проверьте агента с другого узла: `curl https://node-1.example.com:8080/health`. Для локальной разработки можно использовать loopback-узел с `http://127.0.0.1:8080` и отключённым TLS. Не используйте HTTP с bearer-токеном через публичную сеть.

## Несколько узлов

Разверните Compose на каждом сервере. Используйте уникальные `node.id`, разные постоянные тома SQLite и одинаковый список `servers`, `admins`, `AGENT_TOKEN` и `TELEGRAM_BOT_TOKEN`. Порт агента должен быть доступен напрямую с других серверов; VPN и Redis не требуются.

Каждый узел опрашивает агентов всех серверов и пишет метрики в собственную SQLite. Поэтому резервные узлы продолжают накапливать локальную историю. При переходе Telegram-ведущего новый узел использует свою базу; история и пользовательские записи автоматически между узлами не копируются.

Узел с меньшим `priority` предпочтителен среди доступных узлов, если доступно большинство участников кластера. Кандидат проверяет DNS/HTTPS по нескольким адресам и доступность Telegram. Если ведущий теряет большинство или внешнюю связь, он прекращает polling; следующий доступный кандидат принимает роль. Если Telegram сообщает конфликт polling, узел уступает роль.

## Telegram

Создайте бота через BotFather. Администраторы задаются числовыми Telegram ID в `admins`. Пользователь открывает бота после приглашения администратора через `/invite @username`; идентичность затем связывается с числовым Telegram ID из сообщения Telegram.

## GHCR

GitHub Actions форматирует и собирает приложение и Docker-образ при push в `main` и при теге `vX.Y.Z`. Образ публикуется в GHCR с SHA, версией и `latest` для основной ветки.

```sh
docker pull ghcr.io/jsolteam/jsol-uptime-bot:latest
docker compose pull
docker compose up -d
```

Задайте `IMAGE_TAG` для выбора конкретной версии вместо `latest`.

## Ограничения

- Выбор ведущего использует большинство доступных HTTP-агентов и проверки Telegram, но не полноценный консенсус с постоянным голосованием. При асимметричных сетевых сбоях возможна краткая конкуренция; Telegram polling conflict служит дополнительным сигналом, а не строгой блокировкой.
- Если большинство узлов недоступно друг другу, новый ведущий не выбирается, даже если один сервер работает.
- ICMP может блокироваться провайдером или firewall. Ответ агента считается достаточным признаком доступности сервера; отсутствие ping само по себе не переводит его в `DOWN`.
- SQLite и пользовательские доступы локальны. Резервная копия создаётся на каждом узле, автоматической репликации и восстановления на новом ведущем нет.
- Суточная сводка показывает последние состояния и число событий, но не рассчитывает точный SLA и длительность простоев.
