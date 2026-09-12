# HL-240: finish-gate рабочего места агента

Дата проверки: 2026-09-13.

## Вердикт

**PASS для интеграции.** Рабочее место соответствует продуктовой рамке:
оператор сначала видит активный диалог и ход работы, основным действием остаётся
сообщение агенту, а состояние агента и точные идентификаторы отнесены во второй
слой. Незакрытых визуальных дефектов уровня blocker или major в проверенных
сценариях нет.

Это не подтверждает production rollout или работу с живым поставщиком: проверка
выполнена на локальном синтетическом сервере и собранном embedded bundle.

## Проверенная продуктовая рамка

- Desktop: диалоги слева, сообщения и composer в центре, ход работы и вызовы
  инструментов справа.
- Mobile: чат → ход работы → диалоги; в начале доступны переходы «Чат», «Ход
  работы», «Диалоги», «Агент». Диалог и поле сообщения помещаются в первый
  экран 390 × 844 px; история прокручивается внутри чата.
- Показатели и очередь агента скрыты за одной компактной строкой «Состояние и
  очередь агента»; остановка и продолжение очереди остаются быстрыми действиями.
- Разрешение, вопрос агента, остановка и продолжение очереди сформулированы как
  операторские действия.
- UUID, коды событий, версии и hashes видны только в раскрываемых технических
  деталях.

## Finish checks

| Область             | Результат | Evidence                                                                                                                                                                           |
| ------------------- | --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Основной чат        | PASS      | отправка проходит lost-ACK fixture flow; ответ агента отображает Markdown                                                                                                          |
| Ход работы          | PASS      | рядом с чатом видны tool started/output/completed, approval и input request                                                                                                        |
| Состояния           | PASS      | отдельные кадры loading, empty, error; controls flow покрывает waiting input и stopping                                                                                            |
| Responsive          | PASS      | 1280, 1440 и 390 px; page-level horizontal overflow отсутствует; на 390 × 844 чат начинается не ниже 459 px, composer — не ниже 774 px                                             |
| Темы                | PASS      | warm off-white/graphite/cobalt в light; charcoal/slate/cobalt в dark; зелёный, янтарный и красный только семантические                                                             |
| Доступность         | PASS      | touch targets 44 px; видимый 3 px focus ring; проверенные пары текста и фона не ниже WCAG AA 4.5:1                                                                                 |
| Длинный код         | PASS      | fenced code имеет собственный horizontal scroll на 390 px и не расширяет страницу                                                                                                  |
| Markdown safety     | PASS      | raw HTML не исполняется и удаляется renderer-ом; `javascript:` и `mailto:` не становятся ссылками; изображения не загружаются автоматически; user/tool content не интерпретируется |
| Runtime errors      | PASS      | во всех трёх browser-smoke сценариях `pageErrors: 0`                                                                                                                               |
| Anti-pattern review | PASS      | нет gradients, glassmorphism, декоративной KPI-сетки, giant hero или UUID-first presentation                                                                                       |

## Визуальные evidence

- Chat: `/private/tmp/hl240-workspace-ux/harness-desktop.png`,
  `/private/tmp/hl240-workspace-ux/harness-desktop-1440.png`,
  `/private/tmp/hl240-workspace-ux/harness-mobile-light.png`,
  `/private/tmp/hl240-workspace-ux/harness-mobile-dark.png`.
- Tools and waiting input:
  `/private/tmp/hl240-workspace-ux-controls/controls-desktop.png`,
  `/private/tmp/hl240-workspace-ux-controls/controls-desktop-1440.png`,
  `/private/tmp/hl240-workspace-ux-controls/controls-mobile-light.png`,
  `/private/tmp/hl240-workspace-ux-controls/controls-mobile-dark.png`,
  `/private/tmp/hl240-workspace-ux-controls/controls-stopping-mobile.png`.
- State surfaces:
  `/private/tmp/hl240-workspace-ux-states/state-loading-desktop.png`,
  `/private/tmp/hl240-workspace-ux-states/state-empty-desktop.png`,
  `/private/tmp/hl240-workspace-ux-states/state-error-desktop.png`.

## Допустимые остаточные риски

- `react-markdown` и `remark-gfm` увеличили основной minified chunk до примерно
  580 kB; Vite сообщает warning выше 500 kB. Это не функциональный blocker, но
  lazy loading Markdown renderer можно рассмотреть отдельно после измерения
  реальной загрузки.
- Desktop-лента имеет собственный вертикальный scroll, чтобы решения и tool log
  оставались рядом с диалогом; это осознанное поведение workbench.
- При интеграции с параллельным удалением диалогов потребуется вручную свести
  `harness-workspace.tsx` и связанные тесты, затем пересобрать embedded dist.
