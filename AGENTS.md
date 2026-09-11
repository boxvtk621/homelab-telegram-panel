# Правила работы AI-агентов

Действуют для всего `homelab-telegram-panel`. Правила изоляции перенесены из
`homelab-assistant-cursor/AGENTS.md` (commit `1fbdc8e746737d849d3b2a6289e517a5ec5446eb`);
команды и границы адаптированы к отдельному UI + Gateway.

## Работа с Kondor

- Работать как senior/staff engineer + on-call SRE. Общаться по-русски, кратко;
  код, команды и идентификаторы оставлять в стиле репозитория.
- Сначала evidence через файлы, тесты, YouTrack или live-систему, затем вывод,
  действие и повторная проверка. Не выдавать предположение за факт.
- Ask / Plan — чтение, анализ и варианты; не имплементировать без согласия.
  Прямая просьба изменить код — выполнить согласованный scope до проверяемого
  результата. Не останавливать работу на одном плане.
- Data safety важнее скорости. Минимальный diff; без попутного рефакторинга,
  новых сервисов, контрактов или абстракций «на будущее».
- Архитектуру, API/data contract, shared infrastructure, рискованный deploy,
  откат крупного направления и компромиссы сохранности данных согласовывать.
- Commit, push, merge, deploy в любую среду и destructive actions — только при
  явном разрешении пользователя. Разрешение на код не означает разрешения на prod.
- «Готово» — только при evidence по Definition of Done. Сборка не доказывает
  работающий пользовательский сценарий или успешный production rollout.

## Канонические инструкции

- Для HomeLab и `HL-*` использовать MCP `youtrack-homelab`, не сервер другой
  организации `youtrack`. Сначала читать указанную Issue, текущую Task Revision
  и необходимые прямые связи; не загружать весь backlog или Knowledge Base.
- YouTrack — источник требований, решений, revisions, backlog и истории.
  Локальные файлы описывают работу с этим checkout и не заменяют канон.
- Перед разработкой прочитать [CONTRIBUTING.md](CONTRIBUTING.md): там карта
  канонических инструкций HL-A-23/40/586 и HL-A-10/11 для крупных задач,
  команды проверок и правила передачи результата.
- Не копировать полные KB-промпты в `prompts/`, `playbooks/`, `.cursor/rules`
  или другие локальные fallback-файлы. Загружать актуальные статьи адресно.
- Не считать runtime-пути `./workspace`, `./hub` и наличие Controller из
  базового промпта описанием среды разработчика: проверять фактический workspace.
- Не переносить blanket-разрешения и исключения из прошлых Issues в новые задачи.
  Текущие scope/полномочия проверять по диалогу и текущей Issue, а не старому README.

## Обязательная изоляция через Git worktree

- Любая сессия, которая может изменить файлы, зависимости, generated artifacts,
  snapshots, Git refs или runtime репозитория, должна работать в отдельном Git
  worktree.
- Основной checkout (worktree, в котором `.git` является каталогом) используется
  только для read-only диагностики, координации и явно запрошенной интеграции.
  Не создавать в нём рабочие изменения задачи.
- Одна write-сессия = один worktree = одна уникальная ветка. Нельзя запускать две
  write-сессии в одном worktree или продолжать работу в worktree другой сессии.
- Для новой работы создавать ветку с префиксом `codex/` и уникальным именем задачи,
  например `codex/hl-210-panel-topic-20260908`. Не переиспользовать ветку, уже
  занятую другим worktree.
- Если пользователь явно указал существующую ветку, использовать её только в
  отдельном worktree и только когда она не занята другой активной сессией.
- В Codex Desktop выбирать режим Worktree / Hand off → Worktree. Если handoff
  недоступен, допустимо создать отдельный linked worktree через Git и направлять
  все записи и команды сборки в него. Если среда не умеет создать или выбрать
  отдельный worktree, остановиться до изменения файлов и назвать точный blocker.

## Preflight перед первой записью

Получить evidence:

```bash
git rev-parse --show-toplevel
git status --short --branch
git worktree list --porcelain
git symbolic-ref --quiet --short HEAD || true
test -f "$(git rev-parse --show-toplevel)/.git"
```

Запись разрешена только когда одновременно выполнено следующее:

- последняя команда подтверждает linked worktree (`.git` — файл, не каталог);
- worktree принадлежит текущей сессии;
- ветка уникальна для текущей задачи и не checkout'нута в другом worktree;
- исходное состояние чистое либо все имеющиеся изменения доказанно созданы этой
  же сессией.

Detached HEAD допустим только на этапе read-only подготовки Codex-managed
worktree. До первого изменения создать уникальную ветку в этом worktree.

## Границы между сессиями

- Не изменять, не stage'ить, не коммитить и не удалять файлы, созданные другой
  сессией или пользователем.
- Не переносить автоматически незакоммиченные изменения из Local или другого
  worktree. Если выбранная база dirty, сначала сообщить об этом пользователю.
- Не использовать общий `git stash` для передачи работы между сессиями.
- Не выполнять `git add -A` или `git add .`; stage'ить только явно перечисленные
  файлы текущей задачи.
- Не делать checkout/switch/reset/rebase/merge/cherry-pick веток другой сессии без
  явной просьбы пользователя.
- Не удалять и не prune'ить чужие worktree и ветки. Cleanup выполнять только для
  точно установленного worktree текущей сессии и только по явной просьбе.
- Tester/reviewer работает read-only, не продолжает реализацию и не пишет в
  worktree executor. Делегирование ограничивать текущим разрешением и scope.

## Граница Panel, Harness и Fixik

- Panel — рабочее место управления и взаимодействия с независимыми Harness:
  ноды, доступность, занятость, очередь, диалоги, сообщения, история, tool
  timeline и адресные control-команды. Это не UI для issues или Knowledge Base.
- YouTrack — внутренний source of truth требований и политики, к которому агент
  обращается по своему регламенту. Браузер и Panel runtime не требуют, не
  принимают и не хранят YouTrack token.
- Harness владеет исполнением, очередью, историей, SQLite и provider credentials.
  Panel получает только публичные DTO/события и durable receipts через Router;
  она не читает Harness DB и не становится второй очередью.
- Panel, Harness и Telegram-бот/Fixik — независимые приложения. Не добавлять
  общий DB/volumes/secrets, внутренние imports или зависимость запуска.
- Не импортировать внутренние пакеты Fixik, не добавлять sibling-path `replace`
  или зависимость сборки от соседнего checkout. Проверки границ —
  `internal/architecture/boundaries_test.go`.
- Любое действие адресует точные node/dialog/request/attempt IDs и expected
  versions. HTTP acceptance отдельно от длительного выполнения; lost ACK,
  timeout и stale heartbeat не разрешают повтор и не означают idle.
- Сохранять strict edge auth, cookie/CSRF, object isolation, fencing и resource
  bounds. `ownerId` берётся только из подписанного Harness registry и не
  принимается из браузера. Логи не содержат cookies, CSRF, сообщения, секреты
  и raw exceptions.
- Public edge обязан аутентифицировать владельца и перезаписывать
  `X-Panel-Authenticated-User`. Backend доступен только через этот edge;
  `/api/v2/bootstrap` без доверенного header завершается fail-closed.
- Текущий runtime: `internal/panel`, `internal/harnessrouter`,
  `internal/harnessclient`, `internal/harnessprotocol`; session-контракт —
  `api/panel-session.openapi.json`, wire-контракт — `api/harness-v1.schema.json`.
  Старые `mobilegateway*` (кроме assets), `mobileauth`, `mobilecontrollerclient`,
  `mobilecontract`, UI `components/mobile-workspace.tsx` и public API v1 сохранены
  только для parity review/отката. Запрещено подключать их к новой точке входа;
  executable import graph и embedded assets проверяются architecture tests.
- Compose использует собственный bridge, loopback host port и раздельные private
  mounts. `PANEL_HARNESS_COMMANDS_ENABLED` включается только после Router/mTLS/
  ingress проверки. Bridge не заменяет host firewall/egress allowlist.
- Не копировать токены, ключи, локальные MCP/settings, env и production данные
  из Fixik. Не монтировать его DB/secret directory или Docker socket в Panel.
- Logout/expiry/navigation web-сессии не отменяют и не повторяют принятую Harness
  работу. Stop/retry/resume — отдельные exact команды с readback/reconciliation.

## Передача результата

В финальном отчёте указать:

- путь текущего worktree;
- имя ветки и base commit;
- изменённые файлы и выполненные проверки;
- остались ли незакоммиченные изменения;
- что требуется для интеграции результата и что не проверено в runtime.

Если нужно продолжить работу в другой сессии, передавать commit/branch и создавать
для получателя новый отдельный worktree. Не передавать задачу через общий dirty
checkout. Полномочия и нормативные решения сохранять в YouTrack, не в истории чата
или скрытом локальном prompt.
