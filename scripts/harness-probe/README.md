# HL-240: изолированная проверка осуществимости

Пакет V1 из [дорожной карты HL-240](https://youtrack.h1-cloud.ru/issue/HL-240),
рабочие задачи [HL-243](https://youtrack.h1-cloud.ru/issue/HL-243),
[HL-244](https://youtrack.h1-cloud.ru/issue/HL-244) и
[HL-245](https://youtrack.h1-cloud.ru/issue/HL-245).
Это тестовый стенд, не Harness runtime и не новая очередь Panel.
Канонические требования и права остаются в YouTrack; этот файл описывает команды.

## Выполнить V1

Из собственного linked worktree:

```sh
python3 -m unittest discover -s scripts/harness-probe/tests -v
python3 scripts/harness-probe/run.py build --context desktop-linux
python3 scripts/harness-probe/run.py smoke --context desktop-linux \
  --output /tmp/hl240-v1-report.json
```

`--context` обязателен: команда не выбирает случайную текущую Docker-площадку.
Для удалённого Docker нужен предварительно настроенный оператором context.
Нельзя использовать secrets/конфигурацию работающих приложений для сборки.
`--output` создаёт новый файл и отказывается перезаписывать старое evidence.

Сборка использует digest-pinned Debian/glibc base, Node 24.18.0,
Cursor TypeScript SDK 1.0.31, Codex CLI 0.153.4 и npm lockfile. Package install
выполняется без lifecycle scripts. Собранный image ID фиксируется в отчёте;
он относится к конкретной архитектуре сборки, а не обещает проверку другой.

Каждый smoke получает уникальные имена контейнера/volume, UID 10001,
read-only root, `cap-drop=ALL`, `no-new-privileges`, лимит 1 CPU/512 MiB/128 PID,
временный `/tmp` и собственный пустой HOME. Сети, host mounts, Docker socket,
пользовательской конфигурации и provider credentials нет. Контейнеры запускаются
последовательно; существующие workloads не останавливаются. После прогона
удаляются только ресурсы с точным случайным ID текущего запуска; prune отсутствует.

Stdio tool server описан в [tools/README.md](tools/README.md). Его allowlist
служит fixture для следующей проверки deny после resume. Само отсутствие tool
в fixture **не доказывает**, что SDK запретил shell/network или обход через иной tool.

## Как читать результат

`status=passed`, `stage=V1`, `evidenceKind=offline_fixture` доказывают Linux
package import/CLI/schema и deterministic tool tests. `providerCalls=0`,
`providerValidation=not_run`; все обязательные SDK capabilities остаются
`not_run`. Runner отклоняет offline-отчёт, который объявляет live capability PASS,
пропускает движок/обязательный сценарий или сообщает вызов модели.

Codex stable schemas генерируются установленным CLI. Проверяются required fields
`expectedTurnId`, `threadId`, `turnId`; raw schemas/SDK exceptions не включаются
в operational report. Официальный [App Server contract](https://learn.chatgpt.com/docs/app-server)
различает interrupt response и terminal `turn/completed`; эти live исходы
проверяются отдельно в V3. Для Cursor наличие импортируемого API тоже не доказывает
attached steer/cancel/resume в живом запуске.

Если smoke завершился ошибкой, он возвращает ненулевой код и безопасный error code.
Для диагностики trusted fixtures можно запускать `python3 -m unittest` напрямую;
не подставлять реальные credentials в offline image.

## Следующий gate

V2/V3 требуют адресного разрешённого manifest площадки, отдельных provider/KB
credentials, лимита расходов, enforcement test tools и pins. Здесь нет режима,
который по `--live` начинает расходовать account quota: провайдерные сценарии
реализуются и запускаются после проверки этих входов.

## Разрешённая проверка V2/V3

HL-240@13 и HL-243@3 фиксируют разрешение Kondor на локальный Docker и выбор
бюджета координатором. Live-проверки идут последовательно с 1 CPU/512 MiB,
максимум 8 native Codex turn/start и 10 Cursor SDK sends суммарно, 180 секунд на фазу, без автоматических
повторов. Внутренние обращения SDK к модели и usage учитываются отдельно.
После неизвестного исхода runner останавливается и не сбрасывает бюджет.
Cursor limit увеличен root с8 до10 для двух диагностических sends;
предыдущие неизвестные исходы не вычитаются. Решения и offline root cause записаны в HL-246.

`cursor_run.py` принимает точный проверенный image ID `sha256:…`, явный Docker
context, путь к отдельно предоставленному Cursor key (обычный файл0600), новый
output path и обязательный `--prior-sdk-sends`. Значение последнего — число
запросов из предыдущих evidence; для первой попытки0. Key передаётся только
через stdin. Запуск разрешён после source review/image smoke, не через тег.
Runner сохраняет только проверенную схему отчёта после cleanup собственных
контейнеров/volume. В случае timeout количество уже отправленных запросов
может быть unknown: новый запуск требует сверки evidence.

`evidence/cursor-live-v2-01.json`: первые2 SDK sends завершились, marker не
утёк между диалогами; gate unknown из-за неверного сопоставления имени tool.
Cursor1.0.31 обозначает custom tools именем `mcp`; `fixtureEvents` сопоставляет
их с реальным callback по scopeId/callId. Исправление не переименовывает raw
SDK events. Повторный ограниченный прогон должен начинать счётчик с2.

Codex запускается как **штатный `codex app-server`**, который сам ведёт agent
loop и вызывает инструменты. Supervisor/probe управляет native JSON-RPC;
собственной реализации loop поверх Responses API нет. Вход для HL-247 — новый
`codex login --device-auth` внутри отдельного контейнерного `CODEX_HOME`.
Копирование авторизации приложения или Fixik не требуется. Auth volume
содержит чувствительную native session и не включается в сборку/evidence.

`codex_catalog.py` запрашивает только `account/read` и `model/list`, выводит
вид авторизации и безопасный каталог без email/tokens, не делает `turn/start`.
`evidence/codex-auth-catalog.json` подтверждает authMode=chatgpt и доступность
`gpt-5.6-luna` с low effort для synthetic matrix. Такая авторизация расходует
лимиты подписки; API-key mode тарифицируется отдельно. См.
[официальную документацию авторизации](https://developers.openai.com/codex/auth).
Auth/catalog PASS не является V3 capability PASS.

`codex_run.py` требует точный image ID, `--auth-volume`, `--auth-run-id`,
`--model`, `--effort`, `--prior-turn-starts`, `--output` и `--context`.
Перед стартом проверяет owner label и отсутствие другого контейнера с auth volume.
Auth монтируется отдельно от одноразового state. Shell и встроенные Apps выключены;
native effective policy проверяется для каждого нового/возобновлённого thread.
Только три fixture tools имеют точный `approval_mode="approve"`:
`approval_policy="never"` вместе с read-only profile иначе запрещает даже их
безопасные MCP-вызовы. Первый live probe завершился именно таким отказом;
он учтён как2 turn/start, следующая матрица начинается с2 (общий потолок8).
Annotations тестовых инструментов правдиво указывают отсутствие записи и внешних эффектов.
См. [официальную проверку MCP permissions](https://github.com/openai/codex/blob/main/codex-rs/codex-mcp/src/mcp/mod.rs).

Одинаковая обязательная матрица обоих движков: два marker dialogs, tool start/result,
steer во время long tool, terminal после cancel, restart/resume, denied tool после
resume. Обязательный failure/unknown возвращает конкретный факт в HL-243;
не разрешает переход к C1/B1/U1. Synthetic PASS не закрывает пользовательские
сценарии сети/медиатеки, приёмку Linux provider access или весь эпик.

## Принятое live evidence

- Cursor: `cursor-live-v2-02.json` подтверждает5/6 возможностей;
  `cursor-deny-final-03.json` закрывает denyAfterResume: fresh-process resume,
  одна native policy-bearing HTTP/2 request с точным MCP allowlist, terminal
  finished, forbidden builtin/canary отсутствуют. Общая reservation10,
  включая два failed-before-executor supplemental sends.
- Codex: `codex-live-v3-02.json` подтверждает6/6 возможностей, prior2→7/8,
  штатная ChatGPT auth, Luna/low; independent APPROVE.
- Первые unknown отчёты сохранены как provenance, не используются как PASS.
- Usage Codex0.153.4 сбрасывается при новом app-server процессе. В исходном
  live отчёте final53060 — snapshot, не aggregate всей матрицы. Collector
  теперь сохраняет cumulative snapshots по processGeneration/threadId;
  исправление проверено offline без новых model calls. Usage первого
  failed прогона и части отменённых запросов остаётся неизвестным.

Причина supplemental Cursor failure воспроизведена offline: `Agent.create`
резервирует initial queued run; `Agent.resume` не переиспользует этот pending
run, и первый send получает active-run conflict. Подготовка supplemental probe
отменяет только созданную здесь queued запись с пустыми startedAt/checkpoint
через native `Agent.cancelRun`, затем подтверждает cancelled. Это специальная
подготовка synthetic probe, не алгоритм отмены/восстановления реальных задач.

V4 принят в HL-245@4: `evidence/v4-baseline-manifest.json` содержит выбранный
Docker target, политику, расходы и ссылки на evidence. Backup VM116 прошла reboot
и local/cross-host SFTP restore в HL-248@2. M1/G1 завершён; C1 выполняется отдельно
в HL-250. Production capacity, runtime KB/network/media credentials и реальные
пользовательские сценарии остаются следующими gates. Auth volume сохраняется
для дальнейшей согласованной реализации.
