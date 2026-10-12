-- +goose Up
-- The dashboard tile key `watch:` is now `cron:`. Saved dashboards are YAML
-- text, so rewrite the key in every stored body and in every history version
-- (a past version restored later must still parse). The history trigger would
-- record each rewrite as a new version, so it is switched off meanwhile.
-- Block style (`  watch: x`, `- watch: x`) and flow style (`{kind: c, watch: x}`).
ALTER TABLE public.dashboards DISABLE TRIGGER capture_dashboard_history_trg;

UPDATE public.dashboards
   SET yaml_body = regexp_replace(
         regexp_replace(yaml_body, '(^|\n)([ \t]*(-[ \t]+)?)watch:', E'\\1\\2cron:', 'g'),
         '(\{([^}\n]*,)?[ \t]*)watch:', E'\\1cron:', 'g')
 WHERE yaml_body ~ '(^|\n)[ \t]*(-[ \t]+)?watch:'
    OR yaml_body ~ '\{([^}\n]*,)?[ \t]*watch:';

UPDATE public.dashboard_history
   SET yaml_body = regexp_replace(
         regexp_replace(yaml_body, '(^|\n)([ \t]*(-[ \t]+)?)watch:', E'\\1\\2cron:', 'g'),
         '(\{([^}\n]*,)?[ \t]*)watch:', E'\\1cron:', 'g')
 WHERE yaml_body ~ '(^|\n)[ \t]*(-[ \t]+)?watch:'
    OR yaml_body ~ '\{([^}\n]*,)?[ \t]*watch:';

ALTER TABLE public.dashboards ENABLE TRIGGER capture_dashboard_history_trg;

-- +goose Down
-- Not reversible without guessing which `cron:` keys were `watch:` before.
SELECT 1;
