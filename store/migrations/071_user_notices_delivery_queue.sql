-- +goose Up
-- +goose StatementBegin
--
-- user_notices becomes a delivery queue.
--
-- It was a suppression ledger: one row per (user, notice_key), and the row's
-- existence meant "already shown". That shape can answer "has this user seen
-- X?" and nothing else. In particular nothing can *enqueue* a notice, which is
-- what an emitter running outside a request needs to do — migration 072's
-- membership trigger fires on a role change and has to tell an operator
-- something, with no response in flight to hang it on.
--
-- The new model is one row per notice *instance*. seen_at NULL means pending
-- delivery; non-null means shown. Readers ask for pending rows, delivering
-- them marks them seen. There is no suppression layer, no per-key policy and
-- no uniqueness: a notice that shouldn't fire twice is a notice whose emitter
-- shouldn't call it twice. That trade is deliberate — over-firing used to be
-- silently absorbed by the seen-check and is now a visible bug, which is the
-- only way it ever gets fixed.

ALTER TABLE public.user_notices DROP CONSTRAINT user_notices_pkey;
ALTER TABLE public.user_notices ADD COLUMN id BIGSERIAL PRIMARY KEY;

-- seen_at loses NOT NULL and its now() default: NULL is now a meaningful
-- state (pending), and inserting a notice must not mark it delivered.
ALTER TABLE public.user_notices ALTER COLUMN seen_at DROP NOT NULL;
ALTER TABLE public.user_notices ALTER COLUMN seen_at DROP DEFAULT;

-- params carries the facts, not the prose: {"member":"dana@x.com",
-- "groups":["binmgr","configmgr"]}. Presentation stays in the registry in
-- coordinator/notices.go, because the browser and the TUI render differently
-- and a database trigger has no business writing either one's copy.
ALTER TABLE public.user_notices ADD COLUMN params JSONB;

ALTER TABLE public.user_notices
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- The only read path that matters: this user's undelivered notices.
CREATE INDEX idx_user_notices_pending
    ON public.user_notices (user_id) WHERE seen_at IS NULL;

-- No backfill. Every existing row predates the queue and records a notice
-- genuinely shown, so its seen_at timestamp is already correct and it is
-- already excluded from the pending index.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
--
-- Lossy: the old shape holds one row per (user, key), so instances collapse.
-- Pending rows are dropped rather than resurrected as seen — marking a notice
-- delivered that never rendered is the worse of the two lies.
DELETE FROM public.user_notices WHERE seen_at IS NULL;

DELETE FROM public.user_notices a
 USING public.user_notices b
 WHERE a.user_id = b.user_id
   AND a.notice_key = b.notice_key
   AND a.id > b.id;

DROP INDEX IF EXISTS public.idx_user_notices_pending;

ALTER TABLE public.user_notices DROP COLUMN created_at;
ALTER TABLE public.user_notices DROP COLUMN params;
ALTER TABLE public.user_notices DROP COLUMN id;

ALTER TABLE public.user_notices ALTER COLUMN seen_at SET DEFAULT now();
ALTER TABLE public.user_notices ALTER COLUMN seen_at SET NOT NULL;

ALTER TABLE ONLY public.user_notices
    ADD CONSTRAINT user_notices_pkey PRIMARY KEY (user_id, notice_key);
-- +goose StatementEnd
