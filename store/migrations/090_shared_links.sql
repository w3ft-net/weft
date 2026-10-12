-- +goose Up
-- Short links for `clipboard`: a link names a /bin script, a time range and
-- args by a short slug instead of carrying them in the URL. The slug is a
-- hash of (org, script, range, args), so sharing the same thing twice gives
-- the same link. Links resolve only for members of the org that made them.
CREATE TABLE public.shared_links (
    org_id bigint NOT NULL,
    slug text NOT NULL,
    bin text NOT NULL,
    range_spec text NOT NULL,
    args text NOT NULL DEFAULT '',
    created_by_user_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, slug)
);
