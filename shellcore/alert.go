package shellcore

// AlertManPage is the man page for the alert command.
const AlertManPage = `ALERT(1)                    Weft Commands                    ALERT(1)

NAME
    alert - evaluate conditions and dispatch notifications

SYNOPSIS
    <pipeline> | alert [<condition>] [--notify=<alias>]
                       [-k <N>] [-t|--tag <N>]
                       [--duration <dur>] [--average] [--msg <text>]

DESCRIPTION
    Reads a value from stdin and evaluates it against a condition.
    If the condition is met, writes an alert message to stderr and,
    when --notify is supplied, dispatches the alias from /etc/notify.conf
    to its Slack and/or PagerDuty targets.

    If no condition is given, the default is "any": the alert fires
    whenever stdin is non-empty.

    Always passes stdin through to stdout unchanged (tee behavior).

    In a cron watch (/cron.d/), incidents are deduplicated on
    (watch, condition). Subsequent ticks during an open incident
    update it but don't re-page. When the condition clears, the
    incident closes.

    From the interactive shell, alert is the supported way to
    test an alias end-to-end:
        echo "hello" | alert --notify=ops

    Without --notify, alert only writes to stderr — that is the
    dry-run mode.

MULTI-ROW MODE
    By default alert treats stdin as a single scalar value per tick.
    With -k, each non-blank line of stdin is evaluated independently,
    and -k <N> picks the 1-indexed whitespace field that holds the
    numeric value. With -t <N> (a.k.a. --tag), another field names
    the row; each distinct tag opens and closes incidents, and
    dispatches notifications, independently. Use -t whenever the
    pipeline can produce more than one row per tick — otherwise
    multiple rows collapse into the same incident.

    Lines are split on whitespace. Pre-format your pipeline (awk,
    jq, sed) so each row is one line of space-separated values.

CONDITIONS
    any           Fires whenever stdin is non-empty (default)
    above <N>     Current value is greater than N
    below <N>     Current value is less than N
    eq <N>        Current value equals N
    neq <N>       Current value does not equal N
    changed       Value differs from previous run
    absent        No input received in the window
    rising <N>%   Value increased by N% compared to previous/window average
    falling <N>%  Value decreased by N% compared to previous/window average

OPTIONS
    --notify=<alias>
        Dispatch to the named alias in /etc/notify.conf.
        Without --notify, alert only writes to stderr.

    -k <N>
        Multi-row mode: each line of stdin is one row, and field
        N (1-indexed, whitespace-split) holds the numeric value
        for this alert.

    -t <N>, --tag <N>
        Multi-row mode: field N (1-indexed, whitespace-split)
        names this row for per-tag incident dedup and per-tag
        notifications. Required when the pipeline can produce
        more than one row per tick.

    --duration <dur>
        Time window for sustained conditions. For above/below:
        condition must hold for ALL runs in the window. For
        rising/falling: compares against the window average.
        For absent: no non-empty output in the window.
        Examples: 5m, 1h, 30s.

    --average
        For above/below with --duration: compare against the
        rolling average over the window instead of requiring
        every individual run to match.

    --msg <text>
        Override the default alert message.

EXAMPLES
    cat myapp.log --since 5m | awk '$LEVEL == "ERROR"' | alert --notify=ops
    check_errors | alert above 50 --notify=critical
    check_errors | alert above 50 --duration 5m --average --notify=ops
    check_errors | alert rising 100% --notify=critical
    cat /log/canary.log | tail -1 | alert absent --duration 10m --notify=ops
    echo "integration test" | alert --notify=ops

    # Multi-row: alert per host whose disk use is above 80%.
    tail /log/shard.log | jq -r 'select(.type=="disk") | "\(.host) \(.pct)"' \
        | alert above 80 -k 2 -t 1 --notify=ops

SEE ALSO
    help, vi, cat
`
