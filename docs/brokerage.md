# Brokerage Balance Layer — слой брокерских балансов поверх Blnk

> Цель: закрыть функциональный разрыв между Blnk и подсистемой балансов
> TradeControl (раздел 8a project overview): позиции по инструментам,
> settle-модель T+N, праздничный календарь, средневзвешенная цена (WA price)
> с лотами, атомарные мульти-ключевые мутации, FreeBalance и пересчёт
> blocked/waiting из истории транзакций.

---

## 1. Маппинг доменов TradeControl → Blnk

| TradeControl | Blnk (этот слой) | Комментарий |
|---|---|---|
| `Subject` (клиент) | `Identity` (`identity_id`) | как есть в Blnk |
| `Account` (счёт денежный/ЦБ) | `account_ref` (новая колонка на `balances`) | группирует балансы одного счёта |
| `Balance(subject, account, ticker, currency, settle)` | `balances` + новые колонки `instrument`, `settle_date`, `settle_code` | `instrument IS NULL` ⇒ денежный баланс — инвариант TradeControl сохранён |
| `amount` (свободный остаток) | `balance` (= `credit_balance - debit_balance`) | нативное поле Blnk |
| `blockedAmount` | `inflight_debit_balance` | блокировка = незакоммиченный inflight-дебет (hold) |
| `waitingAmount` («в пути») | `inflight_credit_balance` | будущие зачисления = незакоммиченный inflight-кредит (queued_* в blnk считаются на лету и не хранятся) |
| `waPrice` | `wa_price` (новая колонка) | пересчёт по формуле TC, HALF_EVEN, scale 2 |
| `BalanceDetail` (лоты покупок) | таблица `balance_lots` | qty, price, purchased_at |
| `Holiday` / `HolidayService` | таблица `market_holidays` + `ComputeSettleDate` | выходные + праздники площадки не считаются расчётными днями |
| `BalanceKey` / `BalanceDelta` / `BalanceMutationPlan` | `model.BalanceKey` / `BalanceDelta` / `MutationPlan` | детерминированный порядок ключей против deadlock |
| `BalanceMutationServiceImpl.apply` | `Blnk.ApplyMutationPlan` | Redis-локи (по отсортированным ключам) + `SELECT … FOR UPDATE` + batch update |
| `BalanceDistributedLockServiceImpl` (Hazelcast IMap) | `internal/lock` (Redis) | wait/lease конфигурируемы |
| `FreeBalanceAccountService` (Sum0/Sum1/Sum2) | `Blnk.GetFreeBalance` | формула порогов воспроизведена 1:1 |
| `recalculateBlockedWaitingByAllAssetFlows` | `Blnk.RecalculateHolds` | пересчёт inflight/queued полей из транзакций, батчами, ошибки агрегируются по identity |
| `recalculateBlockedWaitingForTrade` (future balance на лету) | `Blnk.BookTrade` + `Blnk.RunSettlement` | букинг создаёт future-баланс T+N; ролл переносит в spot по двойной записи |

### Что сознательно НЕ входит в слой
- **AF471 / регуляторная отчётность** — внешняя интеграция, не задача ledger-ядра.
- **Маршрутизация по `sourceType` EGAR/НТО/BCC** — это интеграционная логика
  TradeControl; здесь дан универсальный механизм (транзакции + mutation plan),
  на который она ложится.
- **UI** — Blnk headless by design.

---

## 1a. Продажа в пути и признак инструмента «trades on the way»

Доступное к продаже количество считается с учётом дат расчётов, но **только
для инструментов с признаком торговли в пути** (`instrument_settings.trades_on_the_way`):

```
on-the-way:  tradable = settled - blocked + incoming(≤ settle_date) - outgoing(≤ settle_date)
immediate:   tradable = settled - blocked            # будущие приход/расход НЕ учитываются
```

- `settled` — спот-остаток; `blocked` — inflight-дебет на споте;
- `incoming`/`outgoing` — суммарный inflight-кредит/дебет на future-балансах,
  созревающих не позже даты расчёта продажи (`SumFutureHolds`).

Если на инструменте нет настроек или флаг выключен — это
immediate-settlement: продать можно только расчётный остаток, приход/расход
в пути игнорируются (`ComputeTradable(..., onTheWay=false)`).

`SellTrade` сначала проверяет `GetTradablePosition` на дату расчёта продажи и
отклоняет сделку при нехватке, затем книжит леги: бумаги — inflight-дебет с
future-позиции на market (`AllowOverdraft`, т.к. покрыто приходом в пути),
деньги — inflight-кредит от settlement.

**Пример (реальный e2e-тест `TestBrokerageChain_...`):** было 100 AAPL,
куплено 50 (T+2), сегодня продаётся 125. AAPL — on-the-way ⇒ tradable =
100 + 50 = 150, продажа 125 проходит; последующие 26 отклоняются (осталось 25).
После расчётов (`RunSettlement`): спот = 100 + 50 − 125 = **25 @ WA 160.00**
(блендинг 100@150 + 50@180 по 150 бумагам). Для immediate-инструмента та же
продажа 125 была бы отклонена (доступно только 100).

### Известное ограничение
Future-балансы идентифицируются по `settle_code` (смещению T+N), а не по
абсолютной `settle_date`. Сделки одного инструмента/счёта с одинаковым T+N в
разные торговые дни попадают в один bucket. Для сценария «в пределах одного
дня» (как выше) это корректно; для мультидневного разделения нужен переход на
ключ по `settle_date` — следующий шаг.

## 2. Семантика settle T+N

- `settle_code = NULL` — текущий (spot) баланс; `settle_code = N` — будущий
  баланс с расчётом `settle_date`.
- **Каскадное чтение** активного баланса (как `getActiveBalanceByAccount`):
  запросили T+2 → если нет, откат T+1 → T+0 → spot (`settle_code IS NULL`).
  Реализовано одним SQL-запросом с `ORDER BY settle_code DESC NULLS LAST LIMIT 1`.
- **Уникальность ключа**: частичный уникальный индекс по
  `(ledger_id, identity_id, account_ref, instrument, currency, settle_code)`
  для брокерских балансов (`account_ref IS NOT NULL`) — дубль активной записи
  невозможен на уровне БД (в TradeControl это runtime-ошибка «Слишком много
  записей…»; здесь — гарантия схемы).
- **`ComputeSettleDate(venue, tradeDate, offset)`** — аналог
  `getSettleCodeAccountingHoliday`: прибавляет расчётные дни, пропуская
  Сб/Вс и праздники площадки из `market_holidays`.

## 3. Жизненный цикл сделки (buy, T+N)

```
BookTrade (POST /brokerage/trades):
  1. settleDate = ComputeSettleDate(venue, tradeDate, N)        # праздники учтены
  2. money-лег:  INFLIGHT debit  client_money → settlement      # blocked ↑
  3. security-лег: INFLIGHT credit market → client_sec(T+N)     # waiting ↑ на future-балансе
     (future-баланс создаётся на лету — как createClearBalanceForRecalculate;
      при сбое security-лега money-лег компенсируется VOID'ом)

SettleTrade (POST /brokerage/trades/:txID/settle) / RunSettlement:
  4. commit money-лега (inflight → applied)                     # blocked ↓, деньги списаны
  5. commit security-лега                                       # waiting ↓, бумаги зачислены
  6. перенос future → spot: обычная двойная запись T+N → T+0
  7. пересчёт WA price + создание лота (на settlement, а не на букинге:
     отменённый трейд не искажает WA — осознанное отличие от TC)

RunSettlement дополнительно: crash-recovery — закоммиченный, но не
перенесённый остаток на созревшем future-балансе докатывается до spot.
```

## 4. WA price (средневзвешенная цена)

Формула TradeControl (`recalculateWawPrice`):

```
WA_new = (tradeMoneyAmount + WA_old × qty_old) / (qty_trade + qty_old)
```

- Округление **HALF_EVEN** (банковское), **scale 2** — воспроизведено на
  `big.Rat` без плавающей точки.
- Одновременно создаётся лот в `balance_lots` (аналог `BalanceDetail`).

## 5. FreeBalance (Sum0/Sum1/Sum2)

Формула `FreeBalanceAccountService.getFreeBalances`:

```
d = Sum1 - Sum2          # доступно минус обязательства
если d < 0        → 0
если Sum0 задан и d ≥ Sum0 → Sum0
иначе             → d
```

По умолчанию: `Sum1 = balance - blocked(inflight_debit)`,
`Sum2 = queued_debit` (обязательства в очереди), `Sum0` — опциональный cap.
Ответ — аналог `FreeBalanceAccountResponse(isSuccess, availableAmount,
errorMessage, currency)`.

## 6. Атомарные мутации (MutationPlan)

Порт подсистемы `balance/mutation/` TradeControl:

- `BalanceKey = (ledger, identity, account_ref, instrument, currency,
  settle_code)`; `LockKey()` — pipe-join.
- `BalanceDelta = (key, amountDelta, blockedDelta, waitingDelta)` c `Merge()`
  и `IsNoOp()`; `MutationPlan.Normalized()` сливает дельты по ключу и
  сортирует по lock key — защита от deadlock.
- `ApplyMutationPlan`:
  1. Redis-локи по всем ключам в отсортированном порядке;
  2. `SELECT … FOR UPDATE` (find-or-create недостающих балансов);
  3. batch update дельт с сохранением инвариантов Blnk
     (`amountDelta>0 → credit_balance`, `<0 → debit_balance`;
     `blocked → inflight_debit`; `waiting → inflight_credit`;
     уход холдов в минус отклоняется);
  4. снятие локов после коммита/отката.
- Это **escape hatch** для пересчётов/реконсиляции — штатные движения денег
  должны идти через транзакции Blnk (как и в TC: «прямые save в обход локов
  чреваты гонками»).

## 7. Пересчёт blocked/waiting из истории

`RecalculateHolds` — аналог `recalculateBlockedWaitingAmountByAllAssetFlows`:
пересобирает `inflight_*`/`queued_*` поля баланса из живых INFLIGHT/QUEUED
транзакций; обрабатывает балансы батчами, параллельно (semaphore),
ошибки агрегируются по identity и логируются сводно.

## 8. REST API слоя

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/brokerage/holidays` | добавить праздник площадки |
| GET | `/brokerage/holidays/:venue` | праздники площадки (фильтр по году) |
| POST | `/brokerage/settle-date` | расчёт settle-даты T+N с учётом праздников |
| POST | `/brokerage/positions` | find-or-create позиционного баланса по ключу |
| GET | `/brokerage/positions/active` | каскадный поиск активного баланса (T+N→…→spot) |
| POST | `/brokerage/instruments` | задать режим инструмента (trades_on_the_way, T+N) |
| GET | `/brokerage/instruments/:instrument` | режим инструмента |
| GET | `/brokerage/positions/tradable` | доступно к продаже (settle-aware, по флагу) |
| POST | `/brokerage/trades` | букинг покупки (hold денег + future-позиция) |
| POST | `/brokerage/sell-trades` | букинг продажи (hold бумаг + приход денег) |
| POST | `/brokerage/trades/:txID/settle` | расчёт bucket'а по security-легу |
| POST | `/brokerage/settlements/run` | ролл созревших future-балансов + коммит холдов |
| POST | `/brokerage/mutations` | применить MutationPlan |
| POST | `/brokerage/balances/recalculate-holds` | пересчёт blocked/waiting из истории |
| GET | `/brokerage/balances/:id/free` | FreeBalance (Sum0/Sum1/Sum2) |
| GET | `/brokerage/balances/:id/lots` | лоты (BalanceDetail) и WA price |

## 9. Схема данных (миграция)

```sql
ALTER TABLE blnk.balances
  ADD COLUMN instrument  TEXT,     -- NULL ⇒ денежный баланс
  ADD COLUMN account_ref TEXT,     -- счёт (группировка балансов)
  ADD COLUMN settle_date DATE,     -- NULL ⇒ spot
  ADD COLUMN settle_code INT,      -- NULL ⇒ spot, N ⇒ T+N
  ADD COLUMN wa_price    NUMERIC;  -- средневзвешенная цена (scale 2)

CREATE UNIQUE INDEX idx_balances_position_key ON blnk.balances
  (ledger_id, identity_id, account_ref, instrument, currency,
   COALESCE(settle_code, -1))
  WHERE account_ref IS NOT NULL;

CREATE TABLE blnk.balance_lots (...);     -- аналог BalanceDetail
CREATE TABLE blnk.market_holidays (...);  -- праздники площадок
```

Существующие балансы Blnk не затрагиваются: новые колонки nullable,
уникальный индекс — частичный (только для брокерских балансов).

## 10. Матрица покрытия раздела 8a TradeControl

| Требование 8a | Статус |
|---|---|
| 8a.1 Модель Balance (ticker/currency/blocked/waiting/waPrice/settle) | ✅ колонки + маппинг на inflight/queued |
| 8a.1 BalanceDetail (лоты) | ✅ `balance_lots` |
| 8a.2 Каскад T+2→T+1→T+0→null | ✅ `GetActivePosition` (один SQL) |
| 8a.2 Уникальность активной записи | ✅ partial unique index (сильнее, чем в TC) |
| 8a.2 Settle-дата с праздниками | ✅ `ComputeSettleDate` + `market_holidays` |
| 8a.3 WA price (HALF_EVEN, scale 2) + лот | ✅ `RecalculateWAPrice` |
| 8a.4 Пересчёт blocked/waiting из проводок, батчи/параллелизм/агрегация ошибок | ✅ `RecalculateHolds` (из транзакций Blnk) |
| 8a.4 Future-баланс «на лету» при трейде | ✅ `BookTrade` |
| 8a.5 MutationPlan: ключи, дельты, merge, ordered locks, FOR UPDATE, batch | ✅ `ApplyMutationPlan` |
| 8a.5 Распределённые локи (wait/lease) | ✅ Redis `internal/lock` |
| 8a.6 FreeBalance Sum0/Sum1/Sum2 | ✅ `GetFreeBalance` |
| 8a.6 FreeBalance корзин (AssetBasket) / долговых | ⚠️ формула дана; маппинг корзин — интеграционный слой |
| 8a.7 AF471 (регуляторка) | ❌ вне скоупа ledger-ядра (внешний сервис) |
| 8a.8 Инварианты (мутации только через план/локи; money vs ЦБ по ticker) | ✅ задокументированы и закреплены схемой |
