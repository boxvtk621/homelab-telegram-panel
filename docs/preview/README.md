# Изображения для README

Скриншоты сняты с tracked embedded UI из commit `535ecc1c6c527b9a5c83a5c41d5304e3f565fe57`.
Исходники интерфейса и его стили не изменялись. `overview.png` — композиция
настольного и мобильного снимков в декоративных рамках, отрисованная Chromium.

| Файл в `../images/` | Сценарий |
|---|---|
| `overview.png` | Обложка 1600 × 1000 |
| `login-desktop.png` | Вход, пустое поле токена |
| `issues-desktop.png` | Список задач, светлая тема |
| `issue-desktop.png` | Описание, поля и обсуждение задачи |
| `knowledge-mobile-dark.png` | База знаний, тёмная тема, viewport 390 × 844 |

Все имена, задачи, статьи, комментарии и сессия синтетические. Скрипт подменяет
API только внутри браузерного контекста Playwright; не запускает backend,
не обращается к реальному YouTrack и запрещает API mutations. Видимая форма
комментария иллюстрирует capability, а не результат проверки записи.
Снимки full-page могут быть выше viewport. Часовой пояс — Europe/Samara.

## Переснять

Работать в собственном linked worktree. Нужны Node.js, Playwright и Chromium.
Можно использовать уже установленный Playwright через `NODE_PATH`, а путь
к Chromium передать через `CHROMIUM_PATH`. Для отдельной установки инструмента
вне зависимостей приложения:

```sh
preview_tools="$(mktemp -d)"
npm install --prefix "$preview_tools" --no-save --package-lock=false playwright@1.62.1
"$preview_tools/node_modules/.bin/playwright" install chromium
NODE_PATH="$preview_tools/node_modules" node docs/preview/capture.cjs
```

Скрипт раздаёт embedded bundle на случайном порту `127.0.0.1`, закрывает браузер
и сервер после съёмки, проверяет отсутствие page errors и горизонтального
переполнения мобильного экрана. При обновлении UI сначала собрать embedded
assets по [CONTRIBUTING.md](../../CONTRIBUTING.md), затем переснять и визуально
проверить изображения; обновить source commit в этом файле и корневом README.
Результат может немного отличаться из-за версии Chromium и системных шрифтов.

Это иллюстрации документации, не Telegram device test, backend acceptance
или свидетельство production rollout.
