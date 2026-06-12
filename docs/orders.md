# Слой ордеров — «Trade Control» поверх брокерского учёта

Документ описывает слой **ордеров с конфигурируемыми статусами и валидацией** —
доменное ядро «Trade Control» (TradeControl `Order`/`OrderCheckService`),
надстроенное над брокерскими сделками (`docs/brokerage_reference.md`).

---

## 1. Идея

Ордер — это **решение/намерение** клиента (купить/продать), которое проходит
жизненный цикл с проверками и согласованием и только затем **исполняется в
сделку** (`BookTrade`/`SellTrade`), которую расчётный движок позже сводит в
позицию. Это отделяет «решение» от «учёта»: до исполнения ордер ничего не
двигает в балансах.

---

## 2. Жизненный цикл и статусы

Статусы — data-driven через таблицу `dict` (TradeControl `Dict`), но канонический
переход закреплён в коде (`model.CanTransitionOrder`):

```
DRAFT ──check──▶ CHECKED ──approve──▶ APPROVED ──execute──▶ EXECUTED
   │                │                    │
   └────────────────┴────────────────────┴──▶ REJECTED / CANCELLED
```

- `DRAFT` — создан, ничего не проверено;
- `CHECKED` — прошёл валидацию (`CheckOrder`);
- `APPROVED` — согласован (`ApproveOrder` — точка для многоступенчатого sign-off);
- `EXECUTED` — исполнен в сделку (`ExecuteOrder`), заполнены `trade_ref`,
  `security_txn_id`, `money_txn_id`;
- `REJECTED` — отклонён (валидацией или при сбое исполнения), с `reject_reason`;
- `CANCELLED` — отменён до исполнения.

`EXECUTED/REJECTED/CANCELLED` — терминальные. Переходы вне схемы → ошибка.
`SubmitOrder` гоняет весь цикл straight-through и останавливается на отклонении.

---

## 3. Валидация (`CheckOrder` → `runOrderChecks`)

Аналог TradeControl `OrderCheckService`, по порядку (первый провал — отказ):

| Проверка | Логика | reject_reason |
|---|---|---|
| **stop_list** | `IsStopListed(identity, instrument)` — блок по идентичности (или глобально `instrument=''`) | `stop list` (+ AML `BLOCKED`) |
| **trading_time** | окно торгов площадки на сегодня (`trading_times`); нет окон у площадки ⇒ открыто | `outside trading hours` |
| **size_limits** | `min/max_order_quantity` из `instrument_settings` (если заданы) | `below/above … order quantity` |
| **funds** | buy: свободный остаток денег (`GetFreeBalance`) ≥ price·qty; sell: `GetTradablePosition` ≥ qty (settle-aware) | `insufficient funds` / `insufficient tradable position` |

Отклонение — **нормальный бизнес-исход** (не ошибка): ордер переходит в
`REJECTED` с причиной, метрика `order.rejected.total{stage=check}`.

---

## 4. Исполнение (`ExecuteOrder`)

`APPROVED` → книжит сделку: buy → `BookTrade`, sell → `SellTrade` (с
`Reference = order.reference`, что делает исполнение идемпотентным). При сбое
букинга (например, гонка, опустошившая доступность между check и execute) ордер
переходит в `REJECTED` (`stage=execute`). Дальше сделка живёт по правилам
брокерского слоя (inflight-холды → `RunSettlement` → позиция).

---

## 5. Таблицы

| Таблица | Назначение |
|---|---|
| `blnk.orders` | ордера: реквизиты, side, quantity/price (NUMERIC, decimal-вход), статус, причины, ссылки на сделку |
| `blnk.dict` | конфигурируемые справочники (статусы, side, …): `(category, code, name, sort, active)` |
| `blnk.stop_list` | блокировки идентичности (по инструменту или глобально) |
| `blnk.trading_times` | окна торгов площадки по дням недели |
| `blnk.instrument_settings` (+колонки) | `min_order_quantity`, `max_order_quantity` |

Миграция: `sql/1781272910.sql` (создаёт таблицы и сидит статусы/стороны).

---

## 6. Сервисы (`*Blnk`, файл `order.go`)

`CreateOrder`, `CheckOrder`, `ApproveOrder`, `ExecuteOrder`, `CancelOrder`,
`SubmitOrder` (straight-through); справочники: `UpsertDict`/`GetDict`,
`AddStopList`, `SetTradingTime`/`GetTradingTimes`.

Метрики: `blnk.brokerage.order.rejected.total{stage}`,
`blnk.brokerage.order.executed.total{side}`.

---

## 7. REST API

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/orders` | создать draft |
| POST | `/orders/submit` | straight-through (create→check→approve→execute) |
| GET | `/orders/:id` | получить ордер |
| POST | `/orders/:id/check` | валидация |
| POST | `/orders/:id/approve` | согласование |
| POST | `/orders/:id/execute` | исполнение в сделку |
| POST | `/orders/:id/cancel` | отмена |
| POST | `/dicts` · GET `/dicts/:category` | справочники |
| POST | `/stop-list` | блокировка идентичности |
| POST | `/trading-times` · GET `/trading-times/:venue` | окна торгов |

---

## 8. Границы / что дальше

- **Согласование** сейчас одношаговое (`ApproveOrder`). Многоступенчатый
  workflow (инициатор→контролёр→комплаенс→юр.отдел, RETURNED) — отдельный слой.
- **AML** — упрощён (PASSED/BLOCKED от стоп-листа); внешний скоринг не подключён.
- **Лимит-/маркет-ордера, частичные исполнения, отмена на бирже** — не
  моделируются (ордер исполняется целиком по своей цене).
- **Комиссии** при исполнении — следующий слой (тарифы).

Проверено сквозными тестами на реальном PostgreSQL (`TestOrderLifecycle_*`):
buy straight-through + settle, пошаговый цикл, sell-ордер, и отказы по
стоп-листу, средствам, лимиту размера и окну торгов.
