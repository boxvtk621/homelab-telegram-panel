# HomeLab Telegram Panel

Самостоятельный репозиторий мобильного интерфейса Fixik: React/Vite UI и Go
Gateway. Постановка и архитектурные решения хранятся в
[HL-210](https://youtrack.h1-cloud.ru/issue/HL-210) и
[HL-A-592](https://youtrack.h1-cloud.ru/articles/HL-A-592).

Извлечён из `homelab-assistant-cursor` на commit
`aea6ae78daf9f7ee42769b6d81cde8b2c37ac5e3`, каталог `next/`.
Это перенос существующей реализации, не завершение всего HL-210.

## Границы

```text
Telegram WebView → HTTPS edge → Panel: UI + Gateway
                                      ↓ private HTTP/JSON over Unix sockets
                                Fixik Controller → DB / workers / YouTrack
```

Panel не содержит Controller, SQLite, worker, ключей шифрования или токенов
YouTrack/бота. Единственные постоянные файлы runtime — бинарник и встроенный UI.
Сессии находятся в памяти; после перезапуска нужна повторная авторизация.
Результаты и задачи сохраняются Controller, а не контейнером Panel.

Public API: `/api/v1`, OpenAPI **1.2.0** в
[api/mobile-workspace.openapi.json](api/mobile-workspace.openapi.json).
Private API: `/internal/mobile/v2`, envelope schema **2**. Клиент проверяет
principal и capabilities в ответах. Несовместимый Controller отклоняется;
успех команды не подтверждается до его durable commit.

`internal/mobilecontract` содержит только проверки wire-формата, не копию
доменного ядра. Go module использует только стандартную библиотеку; sibling
checkout, git submodule и `replace` не нужны. Тест архитектурных границ запрещает
внешние Go imports и подключение ядра через внутренние пакеты.

## Проверка и сборка

Нужны Go 1.26.5 и Node 24.18.0 для совпадения с CI/build images.

```sh
make quality
make image VERSION=local IMAGE=homelab-telegram-panel:local
bash scripts/test-container.sh homelab-telegram-panel:local
```

Quality проверяет Go formatting/vet/race tests, frontend lint/types/tests,
побайтовое соответствие embedded assets чистой сборке и standalone binary.
Dockerfile повторяет Linux-тесты и сборку UI; builder images закреплены digest.
Финальный scratch image не содержит Node, компиляторов, shell или исходников.

## Контейнерный запуск: только та же Linux VM

[compose.yaml](compose.yaml) — fail-closed шаблон, не выполненный deploy.
Значения [.env.example](.env.example) синтетические. Перед запуском необходимо:

1. Выбрать точный image digest и dedicated HTTPS origin; настроить проверенный
   reverse proxy к `127.0.0.1:18080` и независимо отключаемый ingress.
2. На стороне Controller настроить отдельный host UID Panel в peer allowlist,
   socket group и четыре сокета в отдельном socket-only каталоге:
   `application.sock`, `.health`, `.control`, `.recovery`. Каталог и сокеты
   должны быть доступны указанным UID/GID. Не монтировать весь `/run`, БД,
   secret directory или Docker socket. User namespace remapping/rootless
   требуют отдельной проверки фактических peer credentials.
3. Проверить совместимость Controller, ресурсы, monitoring, ограничения egress
   и rollback. `network_mode: host` сохраняет loopback listener, но **не даёт
   сетевой изоляции**. Это не эквивалент `IPAddressDeny` старого systemd unit.
4. Подставить проверенные НЕСЕКРЕТНЫЕ параметры в локальный `.env`, затем
   проверить конфигурацию. Запускать только при готовности целевого окружения.

```sh
docker compose --env-file .env config --quiet
docker compose --env-file .env up -d
```

Контейнер non-root, read-only, без capabilities, с лимитами CPU/RAM/PID/FD.
Mount содержит только сокеты и read-only; TCP-порты не публикуются.
Auto-restart намеренно выключен до приёмки canary.

Для отдельного Proxmox LXC/другой VM этот Compose непригоден: приватный сетевой
transport и его аутентификация ещё не реализованы. Не заменять UDS обычным TCP
и не ослаблять проверки UID для обхода этого ограничения.

## Остаток до релиза HL-210

Перенесённый UI поддерживает чтение, auth/session resume, создание диалога и
submit только за существующими feature flags. В шаблоне обе mutation flags
выключены. Включение требует совместимого Controller и отдельной приёмки.
Ответы/уточнения/отмена, консультация и полный R3 не объявляются готовыми.
Live mobile execution wiring и production/device/rollback acceptance остаются
работой HL-210. Самостоятельная сборка Panel не доказывает их выполнение.

Исходная копия в Fixik пока сохранена: его release tooling ещё собирает старый
Gateway. Удаление этой копии и переключение сборки/деплоя выполняются отдельным
проверяемым шагом; перенос не изменяет текущий production runtime.
