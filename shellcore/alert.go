package shellcore

// AlertManPage is the man page for the alert command.
const AlertManPage = `ALERT(1)                    Weft Commands                    ALERT(1)

NAME
    alert - evaluate conditions and fire notifications

SYNOPSIS
    <pipeline> | alert <condition> [--duration <dur>] [--average]
                                   [--msg <text>] [--send]

DESCRIPTION
    Reads a value from stdin and evaluates it against a condition.
    If the condition is met, writes an alert message to stderr.
    Always passes stdin through to stdout unchanged (tee behavior).

    When used in a cron watch (/cron.d/), stderr triggers notifications
    to configured channels. Incidents are deduplicated: only the first
    trigger fires a notification. Subsequent ticks update the incident
    but don't re-notify. When the condition clears, the incident closes.

    When used interactively, stderr prints to the terminal. Use --send
    to fire a real notification (for testing integrations).

CONDITIONS
    above <N>     Current value is greater than N
    below <N>     Current value is less than N
    eq <N>        Current value equals N
    neq <N>       Current value does not equal N
    changed       Value differs from previous run
    absent        No input received (requires --duration)
    rising <N>%%   Value increased by N%% compared to previous
    falling <N>%%  Value decreased by N%% compared to previous

OPTIONS
    --duration <dur>
        Time window for sustained conditions. For above/below:
        condition must hold for ALL runs in the window. For
        rising/falling: compares against the window average.
        Required for absent. Examples: 5m, 1h, 30s.

    --average
        For above/below with --duration: compare against the
        rolling average over the window instead of requiring
        every individual run to match.

    --msg <text>
        Override the default alert message.

    --send
        (Interactive only) Fire the real notification to
        configured channels instead of just printing to stderr.

EXAMPLES
    check_errors | alert above 50
    check_errors | alert above 50 --duration 5m
    check_errors | alert above 50 --duration 10m --average
    check_errors | alert rising 100%%
    check_errors | alert rising 100%% --duration 10m
    cat /log/canary.log | tail -1 | alert absent --duration 10m
    echo "test" | alert above 0 --send

`
