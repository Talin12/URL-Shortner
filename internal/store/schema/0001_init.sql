-- Phase 1 baseline schema.
--
-- Two tables with deliberately different shapes, mirroring the read/write
-- asymmetry in PLAN.md section 2: links is a small point-lookup table that is
-- the source of truth, click_events is an append-only firehose that is only
-- ever read in aggregate.

CREATE TABLE IF NOT EXISTS links (
    id          BIGINT      PRIMARY KEY,
    code        TEXT        NOT NULL UNIQUE,
    destination TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Codes are handed out from this sequence in phase 1. One round trip per
-- creation and fully enumerable output; both are replaced by the block
-- allocator plus Feistel bijection later (PLAN.md 5.4).
CREATE SEQUENCE IF NOT EXISTS link_id_seq AS BIGINT START WITH 100000 OWNED BY links.id;

CREATE TABLE IF NOT EXISTS click_events (
    id          BIGSERIAL   PRIMARY KEY,
    code        TEXT        NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_agent  TEXT        NOT NULL DEFAULT '',
    referrer    TEXT        NOT NULL DEFAULT ''
);

-- The only query shape the analytics page needs: recent events for one code.
-- This index is also part of why synchronous inserts hurt -- every redirect
-- pays for maintaining it on the hot path.
CREATE INDEX IF NOT EXISTS click_events_code_occurred_at_idx
    ON click_events (code, occurred_at DESC);
