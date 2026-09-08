# HomeLab Panel

Независимое рабочее место AI-агента: React/Vite, Go backend и собственный
Cursor SDK (Python). Telegram-бот для запуска, входа и работы **не нужен**.
Решение владельца: [HL-210@10](https://youtrack.h1-cloud.ru/issue/HL-210),
архитектура: [HL-A-592](https://youtrack.h1-cloud.ru/articles/HL-A-592).

```text
Браузер → Panel → собственный Cursor SDK → инструменты Panel → YouTrack
                      задачи / комментарии / KB                ↑
                                                     Telegram-бот / Fixik
```

Нет вызовов Controller, Unix-сокетов, callbacks, общей БД, volumes, очереди
или bot token. Остановка одного приложения не требует остановки другого.
Общая зависимость — YouTrack: его отказ отключает получение/запись данных,
но не запуск Panel и не выдаётся за отказ бота.

Правила разработки: [AGENTS.md](AGENTS.md), [CONTRIBUTING.md](CONTRIBUTING.md).

## Возможности и честные границы

- Диалог с настоящим Cursor SDK по выбранной задаче, состояние выполнения,
  отдельный ID запуска, остановка и продолжение выбранного завершённого ответа.
  Агент получает задачу и HL-A-23, адресно читает комментарии/KB через четыре
  контролируемых read-tools. В этом инкременте доступна консультация; shell,
  правка кода, deploy и самостоятельные мутации ещё не включены (HL-210).
- Ответ агента можно перенести в черновик комментария, не затирая свой текст;
  публикация остаётся отдельным подтверждённым действием пользователя.
- Список/детали issues проекта, поля YouTrack, страницы комментариев.
- Чтение статей Knowledge Base проекта.
- Запись комментария в точный issue при включённом `PANEL_WRITES_ENABLED`.
- Создание/редактирование задач и статей — по ссылкам в штатный YouTrack.
- Нет прямых команд управления ботом/Controller. Остановка web SDK run
  останавливает только собственный процесс агента и его потомков. Комментарий
  **не означает** admission, выполнение, остановку или изменение Task Revision.
  Автоматический разбор новых issues/comments ботом этим срезом не реализован.
- Обновление по запросу пользователя с временем чтения; не live stream.
  Markdown показывается безопасным текстом, без выполнения HTML.
- Черновики адресованы точному issue и сохраняются в памяти при переключении
  задач/разделов; reload/logout не сохраняет черновики.

Полный продуктовый R1–R3 остаётся HL-210. Изоляция не объявляется полным релизом
старого Mobile Workspace и не доказывает production/device acceptance.

## Независимый вход

Текущий вход — **персональный API-токен YouTrack**, не пароль и не токен бота.
Backend проверяет `/api/users/me` и точный allowlisted `PANEL_OWNER_LOGIN`;
все последующие обращения выполняются от этого пользователя с его правами.
Не передавайте токен агенту, не кладите его в env, Git, URL или web storage.
Вводить его можно только на проверенном dedicated HTTPS origin Panel.
Используйте минимально необходимые права проекта; не административный токен.

YouTrack credential остаётся только в памяти backend-сессии и активного AI-run,
которому пользователь выдал отдельный bounded grant. Браузер получает
opaque `__Host-panel_session` cookie (Secure, HttpOnly, SameSite=Strict) и CSRF
в памяти. Idle TTL 30 минут, абсолютный TTL 8 часов, максимум 4 сессии.
Restart/logout инвалидируют сессии. OAuth/SSO и password login здесь не реализованы.

Внешний proxy должен запрещать body/header logging для login/API, сохранять
точный Host и предоставлять TLS. Backend не пишет HTTP request bodies/headers,
токены, комментарии или upstream errors в журнал.

## API и запись

Текущий контракт: [api/youtrack-panel.openapi.json](api/youtrack-panel.openapi.json),
public prefix `/api/v2`. Контракт AI-запусков:
[api/cursor-agent.openapi.json](api/cursor-agent.openapi.json).
Источником бизнес-контекста остаётся operator-configured HTTPS
YouTrack origin. Redirects/proxy env отключены у YouTrack adapter; timeout/response/page limits,
строгий lossless JSON и проверки проекта/ID обязательны.

Перед записью UI получает session-bound одноразовый permit на точный issue.
Permit потребляется атомарно **до** outbound POST. Повтор/истёкший permit/restart
не инициируют второй POST. Это не durable idempotency или очередь: при потерянном
ответе результат `write_outcome_unknown`, автоматического повторения нет.
Сначала проверить комментарии в YouTrack, затем решать о новой отправке.
Даже успешный ответ означает только запись в YouTrack, не приём агентом.
Проверка проекта перед POST не является транзакционной блокировкой переноса issue
в другой проект; окончательную авторизацию каждой операции выполняет YouTrack.

## Сборка и проверки

Go 1.26.5 / Node 24.18.0, зависимости и build images закреплены.

```sh
make quality
make image VERSION=local IMAGE=homelab-telegram-panel:local
bash scripts/test-container.sh homelab-telegram-panel:local
docker compose --env-file .env.example config --quiet
```

Quality: format/vet/race, frontend lint/types/tests, byte-for-byte embedded build
и standalone binary. Container smoke действительно запускает приложение без
бота и доступного YouTrack, проверяет health=200, data API=401, отсутствие mounts.
Это synthetic/local evidence, не live user acceptance.

## Контейнер

[compose.yaml](compose.yaml): собственный bridge, фиксированный non-root UID,
read-only filesystem, capabilities=none, resource limits, без общих volumes;
собственный tmpfs `/tmp` для временных SDK workspace/state (256 MiB).
Host port опубликован только на 127.0.0.1; внешний TLS ingress не создаётся.
Указывать существующий image digest и проверенные НЕСЕКРЕТНЫЕ параметры из
[.env.example](.env.example). В шаблоне запись выключена.

Можно размещать отдельно от бота. Host networking, peer-UID и socket directory
больше не нужны. Bridge сам по себе не является egress firewall: перед deploy
проверить маршрут/TLS к точному YouTrack, host allowlist egress, proxy logging,
права пользователя, UI login/read/comment, мониторинг и rollback. Compose не
меняет существующую инфраструктуру и не выполняет этот preflight автоматически.

## Релиз образа и установка — HL-239

[release.yml](.github/workflows/release.yml) запускается **только push тега**
`vX.Y.Z` или `vX.Y.Z-rc.N` (N >= 1). Обычный push/PR не публикует образы и не
обновляет сервер. Stable tag должен указывать на commit в `main`; RC допускает
проверку release-кандидата до интеграции. Сначала интегрировать нужные изменения,
проверить exact commit и CI, затем создать новый annotated tag и push именно его.
Не двигать старые теги. Защитить `v*` от изменения/удаления настройками GitHub.

Конвейер: `make quality` → native build/smoke на Linux amd64 и arm64 → GHCR →
multi-platform manifest → GitHub Release с пятью файлами:
`release.json`, `compose.yaml`, `deploy.py`, `panel.env.example`, `SHA256SUMS`.
Версия binary и OCI labels закреплены за tag/commit. Перед публикацией проверяется
отсутствие Release; существующая версия не перезаписывается. Нет mutable `latest`.
Неудавшийся publish может оставить технические `vX.Y.Z-ARCH` tags или manifest без
Release; это не готовый релиз. После опубликованного Release использовать новую
версию, не повторную сборку того же номера.

Образ: `ghcr.io/boxvtk621/homelab-telegram-panel:vX.Y.Z`; устанавливать только
`ghcr.io/boxvtk621/homelab-telegram-panel@sha256:...` из `release.json`.
GHCR publication использует job-scoped `GITHUB_TOKEN` (`packages:write`), создание
Release — `contents:write`. Ключи Cursor/YouTrack и SSH в CI не нужны.
Администратор проверяет доступ workflow к Packages и видимость нового package:
private image требует registry login на Docker-хосте с `read:packages` через
`docker login --password-stdin`/credential store. Секрет не передавать аргументом.
Основа: [GitHub GHCR publishing](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images),
[Docker multi-platform builds](https://docs.docker.com/build/ci/github-actions/multi-platform/).

### Установка или обновление на отдельном Docker-хосте

Требования: Linux amd64/arm64, Python >= 3.10 (stdlib), Docker Engine + Compose v2,
доступ оператора к **локальному default Docker context**, HTTPS reverse proxy на
этом же хосте, сетевой доступ к GHCR/YouTrack/Cursor. Remote Docker contexts и
переменные `DOCKER_HOST`/`COMPOSE_FILE` установщик не использует. Бот не нужен.

Скачать **все пять assets доверенного GitHub Release** в новый каталог. Checksums
обнаруживают повреждение, но не доказывают доверие к издателю: не запускать чужой
`deploy.py`/Compose. Пример (пути на целевом сервере выбирает оператор):

```sh
python3 /opt/panel-release/deploy.py verify --bundle /opt/panel-release
# Только при первой настройке, не поверх существующих credentials:
test ! -e /etc/homelab-panel.env && install -m 600 /opt/panel-release/panel.env.example /etc/homelab-panel.env
# Заполнить /etc/homelab-panel.env через защищённый редактор. Значения в чат/логи не выводить.
python3 /opt/panel-release/deploy.py apply --bundle /opt/panel-release --config /etc/homelab-panel.env --state /var/lib/homelab-panel-deploy --allow-interrupt
```

Config — literal `KEY=value`, без кавычек, `export`, подстановки `$...` и shell
команд. Файл принадлежит запускающему оператору, mode 0600; state-каталог — 0700.
Для первого создания state его родитель должен существовать. Не указывать root,
home или каталог другого приложения. Image выбирает manifest, не config.
Проект Compose детерминированно связан с абсолютным state-путём: всегда повторять
тот же путь, не переносить state как способ смены проекта. Нет общего state бота.

Установщик проверяет пакет, блокирует параллельный запуск, скачивает digest,
сверяет version/revision/UID и валидирует config до замены контейнера. Затем
проверяет точный контейнер/image, binary version, revision, health=200 и anonymous
data=401. Это startup acceptance, **не** проверка live Cursor/YouTrack/внешнего TLS.
`--allow-interrupt` обязателен: текущий runtime теряет активные runs, web sessions
и memory history при замене контейнера. Logout по-прежнему не отменяет run.
Без unattended auto-update/watchtower и без SSH credentials в GitHub.

### Откат и незавершённый deploy

```sh
python3 /opt/panel-release/deploy.py rollback --config /etc/homelab-panel.env --state /var/lib/homelab-panel-deploy --allow-interrupt
```

Сохраняются последняя успешная и предыдущая версии, исходный Compose/checksums и
журнал незавершённой замены. При failed startup автоматически восстанавливается
предыдущий образ и проверяется его запуск; неудавшаяся первая установка удаляет
только собственный service. Ошибка rollback оставляет pending journal и блокирует
новый apply; та же команда `rollback` повторяет восстановление. Не очищать state
вручную для обхода ошибки. Посторонний контейнер или повреждённый пакет дают отказ.

**Rollback возвращает образ/Compose, но не credentials и не историю агента.**
Используется текущий защищённый config. Его содержимое не копируется в state;
хранится только private fingerprint для определения изменений. Повторный apply
того же образа с изменённой config применяет её, а с прежней — только проверяет
текущее состояние. После обновления отдельно проверить login, ответ агента и TLS.
Настоящая публикация GHCR и rollout конкретного хоста фиксируются отдельно от
синтетических тестов delivery tooling; версия образа не означает Feature Done.

## Cursor SDK: настройка и ограничения

Образ содержит Python 3.13.15 и Cursor SDK 1.0.31, не вызывает процесс бота.
`PANEL_CURSOR_API_KEY` предоставляется оператором отдельно через защищённое
окружение сервиса. Не хранить значение в Git, `.env.example`, prompt, CLI argv
или логах. `docker compose config` без `--quiet` при реальной конфигурации может
раскрыть значение — не публиковать вывод. YouTrack token не передаётся в SDK:
HTTPS-запросы выполняет Panel от имени текущего пользователя.

В образе заданы `PANEL_CURSOR_PYTHON=/usr/local/bin/python3`,
`PANEL_CURSOR_WORKER=/opt/panel/worker.py`, `PANEL_CURSOR_MODEL=composer-2.5`.
Модель должна быть доступна аккаунту Cursor. Для локального запуска нужны
Python >=3.10, `pip install -r backend/requirements.txt` в отдельном venv и
абсолютные пути Python/worker в этих переменных. Без ключа AI API возвращает
`cursor_not_configured`; UI YouTrack продолжает работать. Настроенный ключ
не означает успешный model call: отдельно проверить живой ответ и read-tools.

Один активный запуск, максимум 16 записей в памяти, срок запуска 10 минут,
до 40 tool calls. В продолжение передаются последние три завершённых turn;
длинный ответ (>8192 UTF-8 bytes) требует нового диалога. При разрыве связи
панель читает состояние, не запускает агента повторно. Явный retry сохраняет
исходный command ID и payload; idempotency действует для проверенного YouTrack
user ID, пока запись хранится в этом процессе (до восьми часов после terminal).
**Logout и истечение web-сессии не останавливают запущенного агента**: grant на
его YouTrack read-tools и credential живут до окончания этого запуска. Войти
повторно тем же пользователем — увидеть состояние/результат и при необходимости
явно остановить точный run. Чужой user ID не получает эти данные. Закрытие вкладки
также не отменяет работу. После restart сервиса нет автоматического восстановления/replay;
durable multi-dialog/task execution остаётся HL-210, не имитируется здесь.

Выбранные тексты задач, необходимые статьи и история уходят в сервис Cursor:
владелец явно разрешил это в HL-238@2. SDK получает только custom read-tools,
без shell, файловых инструментов, user/project/plugin MCP и настроек бота.
Рабочий каталог каждого запуска отдельный и временный; после запуска удаляется.
SDK/bridge diagnostics не выводятся в логи панели. Production egress должен
учитывать Cursor, а не только YouTrack.

`backend/preflight.py` проверяет установленный SDK и loopback bridge без model
request и credentials; container smoke выполняет его с `--network none`.
Эта проверка не подменяет реальный ответ AI, billing/auth или production rollout.

## Сохранённая предыдущая реализация

Extraction base: Fixik `next/@aea6ae7`; repository base этой миграции `d6121bd`.
Старые gateway/auth/private-client пакеты, UI `components/mobile-workspace.tsx`,
Telegram SDK source и OpenAPI v1 оставлены для сверки паритета и возврата, не удалены.
Они **не импортируются** новым executable/UI, Telegram SDK не поставляется в bundle.
Это проверяется `internal/architecture/boundaries_test.go`.
Историческое имя binary/command сохранено для build tooling; это не runtime связь.

Нельзя случайно вернуть старый entrypoint или включить legacy config.
Source cleanup и новый бот-side YouTrack command protocol — отдельный scope
HL-210/HL-214. Соседний Fixik checkout и текущий production не изменяются.
