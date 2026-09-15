# HL-287: R03 UI conformance finish gate

Дата проверки: 2026-09-15.

## Вердикт

**PASS — UI-01…UI-07, P0=0, P1=0, P2=0 по независимому read-only
re-review. Кандидат ожидает отдельную визуальную приёмку владельца.**

Предыдущий `final9` был отклонён владельцем: он оставлял общий «Ход работы» в
правой колонке и не воспроизводил ключевую композицию `01-dialogue`. Его visual
PASS отозван. Текущий verdict относится только к актуальному незакоммиченному
diff поверх `ebc4ab4c` и evidence `hl287-goal-r04`.

Проверка использует локальные fixtures и embedded bundle. Она не подтверждает
живой Harness/provider, runtime integration, deploy или production rollout.

## Канон и границы

- Issue: HL-287, Task Revision 2; родительский gate — HL-265.
- Base commit: `ebc4ab4c763d936c53691958b806e790526bb04b`.
- Render pack: `render-pack-v1.zip`, SHA-256
  `ba93f4d7707b25c7a08b485738cf547fc98dd791c571d7fd93cc8684bb362612`.
- Визуальный канон: `01-dialogue-{light,dark}.png` и
  `02-harness-{light,dark}.png`.
- Owner override в HL-A-607@2 включает узкий visual slice R04: уже доступные
  `tool.started/output/completed` группируются по exact `attemptId + callId`
  внутри conversation; справа показывается только выбранный вызов.
- API/DTO/store/backend, full safe-text contract и lifecycle/config R09 не
  изменялись. Остальной scope HL-286 остаётся отдельной работой.

## Итоговая композиция

- Persistent desktop rail — 248 px; одна context row — 56 px.
- При viewport 1440 px workspace: rail/dialogs 248 px, conversation 808 px,
  selected-call inspector 384 px. При canonical viewport 1584 px центральная
  область заканчивается ровно на x=1200, как в `01-dialogue`.
- Tool events одного вызова сведены в одну строку. Группа «Действия» находится
  между user message и assistant answer; одинаковые имена разных calls/attempts
  не склеиваются.
- Клик по строке меняет правый inspector. Inspector содержит имя, завершение и
  длительность, вкладки «Результат»/«Вход», тип, операцию и технические IDs.
- Общие request/attempt/approval/input/event controls больше не являются правой
  колонкой. Они доступны из каноничного верхнего `•••`; pending safety gate
  раскрывает их автоматически.
- Management при 1440 px: registry 799 px, inspector 392 px, toolbar 64 px,
  строки 36 px, row action 28 px. Fixture содержит 100 Harness.
- На 720/320 px conversation/composer имеют приоритет; tool inspector,
  управление и dialogs доступны через anchors. Page-level horizontal overflow
  отсутствует; touch targets остаются не меньше 44 px.
- На ширине до 900 px неработающий режим расширения inspector скрыт; на ширине
  до 720 px section/account/context/operations/inspector targets имеют
  фактический bounding box не меньше 44×44 px.

## Structured checklist

| Finding | Verdict | Evidence |
|---|---|---|
| UI-01 shell/composition | PASS | 248/56 shell; conversation + selected-call inspector reproduce `01-dialogue` |
| UI-02 tokens/density | PASS | neutral light/dark themes, 36–40 px rows, 4–6 px radii |
| UI-03 management | PASS | compact 100-row registry, independent selection, DTO-only inspector |
| UI-04 conversation | PASS | exact call grouping inline; selection drives Result/Input inspector |
| UI-05 responsive | PASS | 1440/720/320 matrix, 44 px touch targets, no page overflow |
| UI-06 semantics | PASS | controls and lost-ACK behavior preserved; no implicit POST |
| UI-07 scope | PASS | only owner-approved visual R04 slice; no API/DTO/store/R09 expansion |

## Проверки

- Full pinned `make quality` — PASS: Go vet/race/build, Harness/Agent Service,
  189 release/infra Python tests, frontend suites and reproducible assets.
  Использован repository-compatible Codex CLI `0.153.4`.
- Frontend: schema fixtures 90, Node contracts 125, Vitest 74; lint, typecheck,
  production build и byte-for-byte `make web-check` — PASS.
- Focused `harness-workspace.test.tsx`: 43 tests — PASS. Новый test проверяет
  exact grouping, положение между user/assistant, default selection, смену
  selected call и вкладку Input.
- Chromium/Playwright `chat`, `controls`, `states`, `r03-unknown` — PASS,
  `pageErrors: 0`.
- Независимый первый review нашёл два P2: no-op expand на mobile и поздние CSS
  overrides с targets меньше 44 px. После исправления узкий re-review — PASS,
  UI-01…UI-07 PASS, P0=0, P1=0, P2=0.
- `chat`: 100 Harness; 20 section switches; zero POST до явной отправки;
  desktop geometry 248 + 808 + 384 px; inline group содержит 2 exact calls;
  mobile smoke проверяет скрытый expand и реальные 44×44 bounding boxes.
- `controls`: 3 exact commands, stop остаётся `stopping`; `r03-unknown`: 1
  logical command, 1 status readback, no resend; `states`: loading/error/empty.
- `git diff --check` — PASS.

## Визуальные evidence

Корень:
`/Users/kondor/.codex/visualizations/2026/09/15/01a0a40c-b31c-7391-bc32-68b5cc82498e/evidence/hl287-goal-r04`.

- Exact canonical-size candidate:
  `workspace-reference-1584-{light,dark}.png`,
  `management-reference-1585-{light,dark}.png`.
- Side-by-side, canonical слева / candidate справа:
  `comparison/{dialogue,harness}-{light,dark}-canonical-left-current-right.png`.
- Матрица:
  `{workspace,management}-{desktop-1440,intermediate-720,narrow-320}-{light,dark}.png`.
- Mobile reachability:
  `workspace-{intermediate-720,narrow-320}-{light,dark}-{inspector,dialogs}.png`
  и `workspace-narrow-320-dark-composer.png`.
- Safety flows: `controls/`, `states/`, `unknown/`.

## Незакрытые gate и риски

- Нужна отдельная owner visual acceptance.
- Не проверены живой Harness/provider, runtime integration, deploy и production.
- Minified JS `index-eg_Fax7F.js` — 635.59 kB (gzip 149.69 kB); Vite сохраняет
  предупреждение о chunk больше 500 kB. Оптимизация требует отдельного scope.
- Изменения не закоммичены и не опубликованы. Commit, push и слияние в ветку
  HL-265 — отдельный gate после owner acceptance и явной команды.
