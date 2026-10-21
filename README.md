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
- Linux сервер с публичным IP. Домен и сертификаты не нужны.
- Входящий TCP-порт 8080 для HTTP-агента и ICMP Echo между узлами.
- Одинаковый `AGENT_TOKEN` и токен Telegram-бота на всех экземплярах.

## Запуск

Ниже команды для Ubuntu/Debian. Повторите настройку на каждом сервере. Для связи используются публичные IP-адреса, домены и сертификаты не требуются.

Установите Docker Engine с Compose plugin по [официальной инструкции Docker](https://docs.docker.com/engine/install/). Склонируйте проект, например, в `/opt/jsol-uptime-bot`:

```sh
sudo apt update
sudo apt install -y git
sudo install -d -o "$USER" -g "$(id -gn)" /opt/jsol-uptime-bot
git clone https://github.com/jsolteam/jsol-uptime-bot.git /opt/jsol-uptime-bot
cd /opt/jsol-uptime-bot
```

Скопируйте пример конфигурации и отредактируйте его:

```sh
cd /opt/jsol-uptime-bot
cp configs/config.example.yaml configs/config.yaml
```

В `servers` укажите реальные публичные IP в `address` и `agent_url`, например `203.0.113.10` и `http://203.0.113.10:8080`. Для каждого сервера задайте собственные `node.id` и `node.priority`, а список `servers` оставьте одинаковым. `node.id` и приоритет должны совпадать с записью этого узла в `servers`. Для одиночного узла оставьте в `servers` только его.

Настройки `monitoring.interval`, `timeout`, `failure_threshold` и `recovery_threshold` управляют частотой и устойчивостью проверок. В `monitoring.internet_checks` оставьте минимум два HTTPS URL: это отдельные исходящие проверки интернета с узлов.

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

Вставьте результат `openssl rand -hex 32` как значение `AGENT_TOKEN`. Используйте одинаковое значение на всех узлах. Файл `.env` исключён из Git. Запустите узел:

```sh
docker compose up -d --build
docker compose ps
docker compose logs -f uptime-bot
```

Откройте TCP 8080 во внешнем firewall только для IP других узлов, если провайдер позволяет задать такие правила. На сервере также разрешите ICMP Echo между узлами. `AGENT_BIND_ADDRESS` и `AGENT_PUBLIC_PORT` задают адрес и порт агента; стандартно используется `0.0.0.0:8080`.

Проверьте агента с другого узла: `curl http://203.0.113.10:8080/health`. Трафик агента идёт без шифрования, включая bearer-токен. Ограничьте порт 8080 адресами остальных узлов средствами firewall и используйте длинный случайный `AGENT_TOKEN`.

## Несколько узлов

Разверните Compose на каждом сервере. Используйте уникальные `node.id`, разные постоянные тома SQLite и одинаковый список `servers`, `admins`, `AGENT_TOKEN` и `TELEGRAM_BOT_TOKEN`. Порт агента должен быть доступен напрямую с других серверов; VPN и Redis не требуются.

Каждый узел опрашивает агентов всех серверов и пишет метрики в собственную SQLite. Поэтому резервные узлы продолжают накапливать локальную историю. При переходе Telegram-ведущего новый узел использует свою базу; история и пользовательские записи автоматически между узлами не копируются.

Узел с меньшим `priority` предпочтителен среди доступных узлов, если доступно большинство участников кластера. Кандидат проверяет интернет по нескольким HTTPS-адресам и доступность Telegram. Если ведущий теряет большинство или внешнюю связь, он прекращает polling; следующий доступный кандидат принимает роль. Если Telegram сообщает конфликт polling, узел уступает роль.

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
