# Разработка HomeLab Telegram Panel

Сначала прочитать [AGENTS.md](AGENTS.md). Это developer guide отдельного Panel,
а не инструкции по запуску worker/Controller Fixik.

## Откуда перенесены правила

- Git/worktree/preflight/handoff: `homelab-assistant-cursor/AGENTS.md`, commit
  [`1fbdc8e`](https://github.com/boxvtk621/homelab-assistant-cursor/commit/1fbdc8e746737d849d3b2a6289e517a5ec5446eb).
  Инварианты сохранены; для среды без Desktop handoff описан эквивалент через
  `git worktree add` с записью только в linked worktree.
- YouTrack-first и отсутствие локального prompt/playbook fallback: корневой
  README Fixik на `517eb5a4d657d1a52a8acc10136ae1ab1ca1a151`.
- Проверки/standalone build адаптированы из `next/README.md` и перенесённого
  кода к фактическим целям [Makefile](Makefile) этого репозитория.
- В проверенном Fixik `.cursor/rules` пуст; отдельных отслеживаемых
  `.codex`, `.agents`, `CLAUDE.md` и Copilot instructions нет. Пустые каталоги,
  локальные настройки и deployment scripts Controller не переносились.

## Канонический процесс — читать в YouTrack

Не хранить здесь копии статей или закреплённые старые разрешения. Получать текущие
содержимое и revision через `youtrack-homelab` по точному ID.

| Когда | Статья |
|---|---|
| Вход, режим обращения и адресный выбор инструкций | [HL-A-23 — базовый промпт агента HomeLab](https://youtrack.h1-cloud.ru/articles/HL-A-23) |
| Ведение Issue, scope, план, Task Revision и Done | [HL-A-40 — процесс работы над задачей](https://youtrack.h1-cloud.ru/articles/HL-A-40) |
| Реализация, риск-ориентированные проверки и независимый tester | [HL-A-586 — экономный цикл разработки](https://youtrack.h1-cloud.ru/articles/HL-A-586) |
| Крупная задача, новая архитектура/сервис/API/security boundary | [HL-A-10 — работа над крупными задачами](https://youtrack.h1-cloud.ru/articles/HL-A-10) и [HL-A-11 — анализ задач](https://youtrack.h1-cloud.ru/articles/HL-A-11) |

Для managed work читать HL-A-40 вместе с HL-A-586; для крупной задачи дополнительно
HL-A-10/11. Применение HL-A-586 привязано к точной ссылке/редакции в текущей Issue
и пакете исполнения; это companion HL-A-40, не результат selector HL-A-22/23.
Не загружать все инструкции и backlog «на всякий случай». Ссылки
на эти статьи не утверждают новый scope и не отменяют текущую Issue/revision.

Процесс крупной задачи: бизнес-требования → технический анализ и ТТ/ТУ →
архитектура/ADR → поставляемые Stories → implementation Tasks. Не повторять уже
принятые фазы без evidence изменения входов, но и не выдавать старый candidate
за принятое решение. Упоминание Issue в коде не означает разрешения на её исполнение.

Продуктовые требования Panel: [HL-210](https://youtrack.h1-cloud.ru/issue/HL-210).
Архитектурная граница и информация о выделении репозитория:
[HL-A-592](https://youtrack.h1-cloud.ru/articles/HL-A-592). Статус дополнительных
решений из [HL-A-594](https://youtrack.h1-cloud.ru/articles/HL-A-594) читать явно;
candidate не становится approved из-за наличия реализации.

Сначала зафиксировать scope и 2–4 наблюдаемых пункта «Готово когда». Обычную правку
документа не превращать в новый архитектурный проект. Tester получает точный
diff/commit и DoD, не редактирует файлы; результат review и проверки сохраняются
в текущей Issue. Не изменять существующие KB-инструкции без разрешения на точную
статью. Указанные в старой общей инструкции имена веток не отменяют локальный
стандарт `codex/hl-<номер>-<topic>-<date>` и worktree-изоляцию из AGENTS.md.

## Карта кода — YouTrack-only runtime

- Целевой исполнитель Panel — собственный Cursor SDK (HL-210@10), независимо
  от агента Telegram-бота. `backend/worker.py` и `internal/cursoragent` подключены
  через `internal/panel/agent.go` / `src/panel-agent.tsx`; текущий scope — HL-238@3.
  Не выдавать текущий YouTrack UI за весь AI-продукт.
- `web/mobile-workspace`: исходники React/Vite, клиент API, UI и frontend tests.
- `internal/mobilegatewayassets/dist`: воспроизводимый embedded bundle.
- `internal/panel`: независимый web backend, config, session, CSRF и permits.
- `internal/youtrack`, `internal/strictjson`: HTTPS REST и lossless JSON validation.
- `web/mobile-workspace/src/panel*`: текущий UI и API v2. Main не импортирует
  прежний `components/mobile-workspace.tsx`; Telegram SDK в bundle отсутствует.
- `api/youtrack-panel.openapi.json`: текущий public API contract.
- `Dockerfile`, `compose.yaml`: независимая сборка и не применённый bridge-шаблон.
- `.github/workflows/release.yml`, `scripts/release.py`: tag → tested image →
  GHCR/GitHub Release. `scripts/deploy.py`: operator-run digest deploy/rollback;
  `make release-test` проверяет failure/recovery paths без настоящих credentials.
- Старые mobile gateway/auth/private-client и API v1 оставлены для сравнения
  паритета, но не входят в executable import graph. Их тесты не доказывают новый UI.

Никакие изменения здесь автоматически не изменяют Controller, его БД или
production. Интеграцию в соседний репозиторий выполнять отдельным явным scope.

## Проверки

Из своего linked worktree; версии инструментов зафиксированы в Dockerfile и CI.

```sh
# Go changes: focused tests, затем полный gate по риску изменения.
go test -race ./internal/panel ./internal/youtrack ./internal/strictjson
go test ./internal/architecture

# Полный gate после стабилизации изменения кода/контракта.
make quality

# Container/build changes: отдельный локальный образ, не deploy.
make image VERSION=local IMAGE=homelab-telegram-panel:local
bash scripts/test-container.sh homelab-telegram-panel:local
docker compose --env-file .env.example config --quiet

# Перед передачей/разрешённым коммитом.
git diff --check
git diff --cached --check
```

`make quality` включает formatting, vet, Go race tests, `npm ci` по lockfile,
frontend lint/typecheck/tests, byte-for-byte проверку embedded assets и Go build.
Не запускать несколько одинаковых full gates параллельно. Для документации без
изменения поведения достаточно проверить diff, ссылки/пути/команды и получить
независимое review; не выдавать это за runtime acceptance.

Если менялся UI, сначала `make web-install`, затем из `web/mobile-workspace`
выполнить `npm run build`; он обновляет tracked embedded dist. Проверить через
`make web-check` и включить соответствующие assets в scoped diff. Не редактировать
minified bundle вручную и не добавлять source maps в образ.

При `operation not permitted` на Go cache/локальном сокете проверить sandbox;
повторить те же тесты в разрешённом контексте с отдельным writable `GOCACHE`.
Не отключать тесты или проверку credentials ради зелёного результата.

Smoke проверяет packaging, запуск без бота/YouTrack, health и закрытый data API,
но не реальный login/permissions YouTrack или production egress. Реальный rollout требует
отдельных preflight, canary, мониторинга и проверенного rollback.

## Перед публикацией

Проверить точный source/base, staged scope и отсутствие env/секретов/состояния.
Стадировать явно перечисленные файлы, не `git add .`/`-A`. После разрешённого push
прочитать remote SHA и CI result. Не заменять failed/in-progress CI старым PASS.
В Issue и итоговом ответе разделять code, build, test/review и live release.
