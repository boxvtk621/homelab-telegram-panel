# HomeLab Panel UI

Текущая точка входа `src/main.tsx` → `src/panel.tsx`, API client
`src/panel-api.ts`, stylesheet `src/panel.css`. Backend — независимый Panel,
public API `/api/v2`, upstream только YouTrack.

Вход по персональному YouTrack API-токену; после проверки пользователя UI хранит
только CSRF в памяти и использует HttpOnly cookie. Нет Telegram SDK, bot ID,
initData, Controller API или обязательного Telegram WebView. Credential не
записывается в localStorage/sessionStorage.

Задачи/комментарии/статьи показываются из YouTrack. Комментарий адресован точному
issue; lost ACK не запускает retry. Создание/редактирование задач и KB доступны
по ссылке в штатный YouTrack. Управление execution ботом отсутствует.

`npm run build` воспроизводимо обновляет `../../internal/mobilegatewayassets/dist`.
Только favicon/og и app assets попадают в bundle. Исходники прежнего интерфейса,
его tests и vendored SDK сохранены для parity review, но не импортируются новым UI.
Новый набор сценариев — `tests/panel.test.tsx`; старые тесты не подтверждают новый runtime.

```sh
npm ci
npm run lint
npm run typecheck
npm test
npm run build
```

Из корня: `make web-check` сравнивает чистую сборку с tracked dist.
Runtime/ingress/credentials и ограничения описаны в корневом README.
