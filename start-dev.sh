#!/bin/bash
# Weft dev environment.
#
# Starts the full backend stack via docker-compose (postgres, shards, compute,
# coordinator, API server) and the frontend dev server (Vite) locally.
#
# The Vite proxy forwards /api to localhost:8080 (the dockerized API server).
# The CLI shell uses WEFT_ENDPOINT=localhost:9020 to talk to the dockerized
# coordinator directly over gRPC.
#
# Usage: ./start-dev.sh [-k|--kill-ports]

set -euo pipefail

APP_NAME="weft-dev"
APP_DIR="$(cd "$(dirname "$0")" && pwd)"
FRONTEND_PORT="5173"

# Parse args
KILL_PORTS=false
while [[ "$#" -gt 0 ]]; do
    case $1 in
        -k|--kill-ports) KILL_PORTS=true ;;
        *) echo "Unknown parameter: $1"; exit 1 ;;
    esac
    shift
done

if [ "$KILL_PORTS" = true ]; then
    echo "Killing processes on port $FRONTEND_PORT..."
    lsof -ti:$FRONTEND_PORT | xargs kill -9 2>/dev/null && echo "  Killed $FRONTEND_PORT" || echo "  Port $FRONTEND_PORT free"
    echo ""
fi

# Pane commands
_backend_cmd="cd $APP_DIR && docker compose up --build 2>&1"

_frontend_cmd="cd $APP_DIR/frontend && npx vite --port $FRONTEND_PORT"

_shell_cmd="cd $APP_DIR && sleep 15 && WEFT_ENDPOINT=localhost:9020 WEFT_CUSTOMER=kevlar go run ./cmd/weft"

# Layout: 3-pane (backend left, frontend top-right, shell bottom-right)
_make_layout() {
    local w="$1"
    tmux split-window -h -t "$w"
    tmux select-pane -t "${w}.1"
    tmux split-window -v -t "$w"
}

_fill_panes() {
    local w="$1"
    tmux send-keys -t "${w}.0" "$_backend_cmd" Enter
    tmux send-keys -t "${w}.1" "$_frontend_cmd" Enter
    tmux send-keys -t "${w}.2" "$_shell_cmd" Enter
    tmux select-pane -t "${w}.2"  # focus shell pane
}

if [ -n "${TMUX:-}" ]; then
    SESSION=$(tmux display-message -p '#S')
    WINDOW="${SESSION}:${APP_NAME}"
    if tmux list-windows -t "$SESSION" | grep -q "^[0-9]*: ${APP_NAME}[* -]"; then
        echo "Window '${APP_NAME}' already exists. Switching to it..."
        tmux select-window -t "$WINDOW"
        exit 0
    fi
    tmux new-window -n "$APP_NAME"
    _make_layout "${SESSION}:${APP_NAME}"
    _fill_panes "${SESSION}:${APP_NAME}"
else
    if tmux has-session -t "$APP_NAME" 2>/dev/null; then
        echo "Session '$APP_NAME' already exists. Attaching..."
        tmux attach-session -t "$APP_NAME"
        exit 0
    fi
    tmux new-session -d -s "$APP_NAME" -n "dev"
    _make_layout "${APP_NAME}:dev"
    _fill_panes "${APP_NAME}:dev"
    tmux attach-session -t "$APP_NAME"
fi

echo ""
echo "$APP_NAME started."
echo "  Frontend:    http://localhost:$FRONTEND_PORT"
echo "  API:         http://localhost:8080 (docker)"
echo "  Coordinator: localhost:9020 (docker, gRPC)"
echo "  Postgres:    localhost:5433 (docker)"
echo ""
echo "CLI shell: WEFT_ENDPOINT=localhost:9020 go run ./cmd/weft"
echo ""
