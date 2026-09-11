# HomeLab Panel UI

Текущая точка входа `src/main.tsx` → `src/panel.tsx`, API client
`src/panel-api.ts`, stylesheet `src/panel.css`. Backend — независимые Panel и
Harness Router, public API `/api/v2`.

Публичный edge уже аутентифицирует владельца. UI автоматически обменивает
доверенный edge header на opaque `HttpOnly` cookie и держит только CSRF nonce в
памяти React. Браузер не передаёт `ownerId`, YouTrack token или provider
credential.

Основной путь: список Harness-нод → точная нода → долговечный диалог → durable
receipt → очередь/события/ответ → continuation или адресный control. Возврат к
management не размонтирует активное interaction-состояние и не повторяет
принятую команду.

`npm run build` воспроизводимо обновляет `../../internal/mobilegatewayassets/dist`.
Только favicon/og и app assets попадают в bundle. Исходники прежнего mobile
workspace сохранены для parity review, но не импортируются Panel UI.

```sh
npm ci
npm run lint
npm run typecheck
npm test
npm run build
```

Из корня: `make web-check` сравнивает чистую сборку с tracked dist.
Runtime/ingress/credentials и ограничения описаны в корневом README.
