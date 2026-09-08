# Fixik Next Mobile Workspace

React/Vite-клиент для HL-210. Production build записывается в tracked
`../../internal/mobilegatewayassets/dist/`, встраивается в standalone Gateway
через `go:embed` и раздаётся с того же exact HTTPS origin, на котором расположен
`/api/v1`. Runtime-файлы рядом с бинарником не требуются.

Клиент читает только строгий schema v1 и реализует:

- Telegram `initData` bootstrap и HttpOnly web-сессию;
- owner-scoped Dialog, decrypted message и Task readback;
- management-event delta polling и явный stale/offline режим;
- read-only Control, liveness/readiness и capacity;
- отдельный Task detail: клик передаёт exact Task ID, после чего клиент
  выполняет owner-scoped `GET /api/v1/tasks/{task_id}`; terminal Task не зависит
  от `Dialog.active_task_id` после освобождения work slot;
- loading, empty, auth-expired, permission и partial-outage состояния;
- bounded черновик на каждый Dialog только в памяти/sessionStorage текущей
  вкладки.

Статических task/dialog/control данных нет. Создание пустого диалога доступно
только при `FIXIK_NEXT_MOBILE_DIALOG_CREATION_ENABLED=true` на обеих сторонах и
подтверждённом session capability. `POST /session/resume` восстанавливает CSRF
после переоткрытия WebView без повторного initData; Origin, cookie и browser
client ID обязательны, абсолютный срок сессии не продлевается.

До POST клиент сохраняет только command ID и expected collection version в
localStorage. Web Locks сериализует эту запись и запрос между вкладками;
без Web Locks создание выключено. Потерянный ACK сохраняет тот же command ID
для явного повтора после reload. Конфликт требует обновления списка и нового
явного действия. Поля actor/client/capability не принимаются в public JSON.
Task submit включается отдельно флагом `FIXIK_NEXT_MOBILE_TASK_SUBMISSION_ENABLED`
на обеих сторонах. Он требует dialog creation, явного YouTrack issue ID и
сохраняет тот же command ID при повторе. `received` не означает `accepted`:
admission и execution принадлежат Controller. Cancel остаётся недоступен;
клиент не показывает фиктивный успех. Код приложения не сохраняет `initData`,
session cookie или CSRF в web storage; session cookie остаётся HttpOnly.
Vendored Telegram SDK сохраняет Telegram launch params в `sessionStorage` по
своему upstream-протоколу, но приложение не использует `initDataUnsafe`.

`window.Telegram.WebApp` предоставляет vendored official SDK v63 из
`public/telegram-web-app.js`. Проверенный source URL:
`https://telegram.org/js/telegram-web-app.js?63`; размер 116510 bytes, SHA-256
`3549138a7934039fe7dfd1291a4ee739bd2b705a614308053a8b08a87d85c451`.
`index.html` дополнительно фиксирует тот же digest через SRI. Runtime-загрузки с
`telegram.org` нет: production CSP достаточно `script-src 'self'` и
`connect-src 'self'`. Клиент не использует `initDataUnsafe`.

При upgrade SDK нужно получить новую exact-версию только с official URL,
проверить diff и размер, зафиксировать новый SHA-256/SRI в этом README и
`index.html`, затем повторить lint, typecheck, production build и audit.

Sites/Vinext/Cloudflare Worker намеренно не используются: hosted runtime не
может сохранить локальную UDS-границу Gateway → Controller. Production ingress
и Telegram device validation остаются release gates ADR-MW-002.

Telegram `start_param`/deep-link navigation в R0 не реализована: соответствующий
контракт ещё не frozen. При её добавлении параметр может быть только untrusted
navigation hint; существующий owner-scoped server GET и его IDOR-проверка
останутся единственным authority для открытия Task/Dialog.

## Локальная проверка

```bash
npm ci
npm run dev
npm run lint
npm run typecheck
npm test
npm run build
npm audit
```

Из корня репозитория команда `make web-check` выполняет чистую сборку во временный
каталог и побайтово сравнивает её с tracked embedded dist. `make
web-quality` дополнительно выполняет lint, typecheck и тесты. Source maps
в artifact запрещены; fingerprinted JS/CSS получают immutable cache, а HTML —
`no-store`.
