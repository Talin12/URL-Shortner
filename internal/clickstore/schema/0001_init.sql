-- ClickHouse schema for click events.
--
-- The shape differs from the Postgres table in ways that matter:
--
--   ORDER BY (code, occurred_at) is the primary key AND the physical sort
--   order. Every analytics query is "events for this code, over this window",
--   so sorting by code first means one contiguous read rather than an index
--   lookup per row.
--
--   PARTITION BY month makes retention a metadata operation: dropping old
--   click data is a DROP PARTITION, not a DELETE over billions of rows.
--
--   There is no primary key per row and no uniqueness constraint. Click events
--   are append-only and approximate by design (PLAN.md section 2), so paying
--   for either would be paying for a guarantee this workload does not want.

CREATE TABLE IF NOT EXISTS click_events
(
    code        String,
    occurred_at DateTime64(3, 'UTC'),
    user_agent  String,
    referrer    String
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (code, occurred_at);

-- Pre-aggregated daily totals. SummingMergeTree collapses rows with the same
-- sort key in the background, so this table stays small no matter how many
-- raw events arrive.
CREATE TABLE IF NOT EXISTS click_counts_daily
(
    code   String,
    day    Date,
    clicks UInt64
)
ENGINE = SummingMergeTree
ORDER BY (code, day);

-- The rollup is maintained on insert rather than on read. A stats query then
-- touches a handful of pre-summed rows instead of scanning the raw events,
-- which is the whole reason a columnar store earns its place here.
CREATE MATERIALIZED VIEW IF NOT EXISTS click_counts_daily_mv
TO click_counts_daily
AS
SELECT
    code,
    toDate(occurred_at) AS day,
    count() AS clicks
FROM click_events
GROUP BY code, day;
