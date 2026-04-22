package shellcore

// AlertManPage is the man page for the alert command.
const AlertManPage = `ALERT(1)                    Weft Commands                    ALERT(1)

NAME
    alert - evaluate conditions and dispatch notifications

SYNOPSIS
    <pipeline> | alert [<condition>] [--notify=<alias>]
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

SEE ALSO
    help, vi, cat
`
