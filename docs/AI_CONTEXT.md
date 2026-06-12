# AI Context — карта репозитория для анализа

> Назначение: дать ИИ-агенту (или новому разработчику) быстрый и точный контекст
> по этому репозиторию, чтобы анализ/доработки шли без повторного «разбора с
> нуля». Читать **первым**. Язык кода — English; документация — русская.

---

## 1. Что это за репозиторий

- **Основа:** клон открытого проекта **blnkfinance/blnk** — ledger двойной
  записи на Go (см. `README.md`, `ОПИСАНИЕ.md`). Импортирован одним коммитом;
  история upstream не тащилась.
- **Что добавлено поверх:** **брокерский слой балансов** — порт подсистемы
  балансов системы TradeControl (back-office брокера) на примитивы Blnk:
  позиции по инструментам, расчёты T+N, блокировки/приход «в пути»,
  средневзвешенная цена, мультидневный учёт.
- Модуль Go: `github.com/blnkfinance/blnk`, Go 1.25. Это **тот же** module path,
  что у upstream (импорты не меняются).

### С чего начать чтение (по приоритету)
1. `docs/brokerage_reference.md` — **подробное** описание брокерского слоя:
   сервисы, таблицы, значения, логика расчётов, **8 Mermaid-диаграмм**.
2. `docs/brokerage.md` — дизайн и маппинг «TradeControl → Blnk», матрица покрытия.
3. Этот файл — карта кода, инварианты, как собирать/тестировать.
4. `ОПИСАНИЕ.md` — обзор самого Blnk (на русском).

---

## 2. Где что лежит (брокерский слой)

Весь добавленный функционал помечен именем `brokerage` и сосредоточен в:

| Файл | Слой | Содержимое |
|---|---|---|
| `model/brokerage.go` | домен (без БД) | `PositionKey`, `BalanceDelta`/`MutationPlan`, `ComputeSettleDate`, `RecalculateWAPrice`, `ComputeTradable`, `FreeBalance`, `InstrumentSettings`, структуры сделок |
| `model/brokerage_test.go` | тесты | чистая логика (арифметика, ключи, план мутаций) |
| `database/brokerage.go` | доступ к данным | CRUD позиций по ключу-дате, каскад, `ApplyBalanceDeltas` (FOR UPDATE), `RecomputeHolds`, лоты, праздники, настройки инструмента, `SumFutureHolds` |
| `database/brokerage_test.go` | тесты | sqlmock |
| `database/repository.go` | интерфейс | `IDataSource` включает интерфейс `brokerage` (методы перечислены там) |
| `database/mocks/repo_mocks.go` | моки | `MockDataSource` реализует все методы (в т.ч. brokerage) |
| `brokerage.go` (корень) | сервис `*Blnk` | `BookTrade`, `SellTrade`, `SettleTrade`/`RunSettlement`, `GetTradablePosition`, `ApplyMutationPlan`, `RecalculateHolds`, `GetFreeBalance`, резолв даты/режима |
| `brokerage_test.go` | тесты | сервис на mock + miniredis |
| `brokerage_integration_test.go` | тесты | **сквозные на реальном PostgreSQL** (под флагом, см. §5) |
| `api/brokerage.go` | HTTP | хендлеры `/brokerage/*` |
| `api/model/brokerage.go` | HTTP | DTO + валидация |
| `api/api.go` | роутер | регистрация маршрутов `/brokerage/*` (искать комментарий «Brokerage routes») |
| `sql/1781243214.sql` | миграция | колонки `account_ref/instrument/settle_date/settle_code/wa_price`, таблицы `balance_lots`, `market_holidays` |
| `sql/1781246704.sql` | миграция | таблица `instrument_settings` |
| `sql/1781256443.sql` | миграция | перевыпуск уникального индекса ключа позиции на `settle_date` |
| `sql/1781270635.sql` | миграция | индекс `idx_balances_settle_date` по `settle_date` (а не `settle_code`) |
| `sql/1781270771.sql` | миграция | таблица `brokerage_settlement_journal` (двухфазный recovery расчёта) |
| `sql/1781270772.sql` | миграция | идемпотентные лоты (unique `balance_lots(balance_id, reference)`) |
| `internal/metrics/brokerage.go` | метрики | brokerage-инструменты (booked/rejected/settlement/reconciled/latency) |

Модель данных и слои наглядно — в диаграммах `docs/brokerage_reference.md` §11.

---

## 3. Ключевые инварианты (читать перед правками!)

1. **Позиция = баланс Blnk** в `blnk.balances`, размеченный колонками
   `account_ref`/`instrument`/`settle_date`. `account_ref IS NULL` ⇒ это
   «родной» баланс Blnk, брокерский слой его не трогает.
2. **Деньги vs бумаги:** `instrument IS NULL` ⇒ денежная позиция; задан ⇒
   позиция по бумагам.
3. **Идентичность позиции** = `(ledger, identity, account_ref, instrument,
   currency, COALESCE(settle_date,'1970-01-01'))`. Уникальный частичный индекс
   `idx_balances_position_key`. **`settle_date`, а не `settle_code`** — это
   ключ мультидневного учёта.
4. **`settle_code`** (смещение T+N) — справочный, может быть `NULL` (N не
   контролируется). Не использовать как идентичность.
5. **Маппинг inflight ↔ доменные понятия:**
   - `inflight_debit_balance` = **блокировка** (`blockedAmount` в TradeControl);
   - `inflight_credit_balance` = **приход «в пути»** (`waitingAmount`).
6. **Доступно к продаже** считается с учётом даты расчёта **только** для
   инструментов с `trades_on_the_way = true`; иначе — только settled − blocked.
7. **WA-цена** обновляется на **расчёте покупки** (не на букинге); продажа по WA
   среднюю не меняет. Округление HALF_EVEN, scale 2, арифметика на `decimal`.
8. **Расчёт = net-roll:** будущая позиция нетится (`balance = credit − debit`),
   результат переносится в спот обычной транзакцией; будущая позиция → 0.
9. **Штатные движения** денег/бумаг идут через **транзакции Blnk**
   (`RecordTransaction`/inflight), а не через `ApplyMutationPlan` (последний —
   только для ручных корректировок под распределённой блокировкой).
10. **Расчёт идемпотентен и восстановим.** Побочные эффекты расчёта покупки
    (`wa_price` + лот) пишутся атомарно через двухфазный
    `brokerage_settlement_journal`: pending-строка (с `wa_before`/`qty_before`)
    до коммита легов, `applied` — в одной транзакции с лотом и ценой.
    `RunSettlement` сначала вызывает `ReconcileSettlement` (доводит pending),
    `GetMaturedPositions` пропускает обнулённые bucket'ы. Лоты идемпотентны по
    `(balance_id, reference)`.
11. **Числа на входе — строки/int, не float.** Брокерское API принимает
    `quantity` строкой (decimal) и `*_precision` целыми; value-путь идёт через
    `model.PreciseQuantity`/`PreciseMoney` → `big.Int`. Float64 в денежных/
    количественных значениях не используется.

---

## 4. Подводные камни Blnk (выяснены на практике)

> Это поведение **самого Blnk**, не очевидное из кода. Учитывать при любых
> прямых вызовах `RecordTransaction`.

1. **Синхронный путь `SkipQueue=true` не генерирует `transaction_id`.** ID
   присваивается только в очередь-пути (`transaction_queue.go`). При прямом
   синхронном вызове нужно задавать `TransactionID` самому
   (`model.GenerateUUIDWithSuffix("txn")`), иначе два insert'а столкнутся на
   пустом `transaction_id` (duplicate key). В брокерских легах ID уже ставится.
2. **`AmountString` не заполняется на пути `Amount → PreciseAmount`.** Колонка
   `transactions.amount` — NUMERIC; insert пишет `txn.AmountString`. Если задан
   только `Amount` (float) без `PreciseAmount`, `AmountString` остаётся пустым
   ⇒ `invalid input syntax for numeric ""`. Брокерские леги задают
   `AmountString` явно. То же касается тестовых fund-переводов.
3. **Inflight-дебет проверяет доступность** `balance − inflight_debit` и
   отклоняет нехватку без `AllowOverdraft`. Брокерские леги, опирающиеся на
   приход «в пути» (продажа) или на контрагента (market), ставят
   `AllowOverdraft: true` — экономика уже проверена `GetTradablePosition`.
4. Числовые колонки балансов/транзакций — **NUMERIC** (расширены из BIGINT
   более ранней миграцией upstream); читать как текст и парсить через `big.Int`.

---

## 5. Сборка и тесты

```bash
go build ./...
go vet ./...
go test ./model/ ./database/ ./api/model/   # юнит без внешних сервисов
```

- Часть **upstream-тестов требует живых сервисов** (PostgreSQL/Redis на
  localhost) и падает без них — это **не** связано с брокерским слоем (можно
  проверить: те же тесты падают на чистом upstream-коммите).
- **Сквозные брокерские тесты** (`brokerage_integration_test.go`) идут на
  реальном PostgreSQL и **скипаются**, если не задан
  `BLNK_TEST_DATA_SOURCE_DNS`. Запуск с локальным PG (как unprivileged user):

```bash
# поднять временный PostgreSQL 16
useradd -m pg 2>/dev/null; chmod 777 /tmp
su pg -c "/usr/lib/postgresql/16/bin/initdb -D /tmp/pgdata -U postgres -A trust"
su pg -c "/usr/lib/postgresql/16/bin/pg_ctl -D /tmp/pgdata -o '-p 55432 -k /tmp' -l /tmp/pg.log start"
psql -h /tmp -p 55432 -U postgres -c "CREATE DATABASE blnk;"

# применить миграции (Up-секции) напрямую, минуя redis-зависимый `migrate`:
export PGHOST=/tmp PGPORT=55432 PGUSER=postgres
for f in $(ls sql/*.sql | sort -t/ -k2 -n); do
  awk '/^-- \+migrate Up/{m="up";next} /^-- \+migrate Down/{m="down";next}
       /^-- \+migrate Statement/{next} {if(m=="up")print}' "$f" \
  | psql -d blnk -v ON_ERROR_STOP=1 -q
done

# запустить сквозные тесты
export BLNK_TEST_DATA_SOURCE_DNS="postgres://postgres@/blnk?host=/tmp&port=55432&sslmode=disable"
go test . -run TestBrokerageChain -v
```

- Между прогонами интеграционных тестов чистить данные:
  `TRUNCATE blnk.balances, blnk.transactions, blnk.balance_lots,
  blnk.market_holidays, blnk.instrument_settings, blnk.ledgers CASCADE;`
- Команда `go run ./cmd migrate up` требует Redis (создаёт полный `Blnk`);
  для одной только схемы удобнее применять Up-секции через psql, как выше.

### Формат миграций
`sql-migrate` (`github.com/rubenv/sql-migrate`), встроены через
`//go:embed sql/*.sql` в `blnk.go`. Файлы — `<unixtime>.sql`, секции
`-- +migrate Up` / `-- +migrate Down` (несколько Up/Down-блоков допустимо).

---

## 6. Технологический стек

Go · Gin (HTTP) · PostgreSQL (`lib/pq`/pgx) · Redis (`go-redis`, asynq) ·
Typesense (поиск) · Prometheus/OpenTelemetry/Jaeger (наблюдаемость) ·
`shopspring/decimal` (денежная арифметика) · `sqlmock`/`miniredis`/`testify`
(тесты). Схема БД — в namespace `blnk.*`.

---

## 7. Что НЕ входит в брокерский слой (осознанные границы)

- AF471 и регуляторная отчётность — внешняя интеграция.
- Маршрутизация EGAR/НТО/BCC и оркестрация процессов (Camunda) — это слой
  TradeControl, не ledger-ядро.
- Потребление лотов FIFO/LIFO при продаже не моделируется; авторитетная
  средняя — `wa_price` на балансе.

---

## 8. Git / рабочий процесс

- Рабочая ветка: `claude/compassionate-bardeen-is9vpm`.
- Коммиты атомарные, с осмысленными сообщениями; брокерский слой добавлялся
  инкрементально (модель → БД → сервис → API → тесты; затем продажи и
  settle-aware доступность; затем мультидневный учёт по `settle_date`; затем
  документация и диаграммы).
- Перед изменением поведения балансов — свериться с инвариантами §3 и
  подводными камнями §4, прогнать сквозной тест из §5.
