# Shared helpers for scripts/dev/{up,down}.sh. Source, don't run.
# PIDs live in var/run/<name>.pid, logs in var/log/<name>.log.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RUN_DIR="$ROOT/var/run"
LOG_DIR="$ROOT/var/log"
mkdir -p "$RUN_DIR" "$LOG_DIR"

pid_alive() { # name
  local f="$RUN_DIR/$1.pid"
  [[ -f "$f" ]] && kill -0 "$(cat "$f")" 2>/dev/null
}

wait_http() { # url seconds [expected-status]
  local url=$1 secs=$2 want=${3:-200} i code
  for ((i = 0; i < secs * 4; i++)); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$url" || true)
    [[ "$code" == "$want" ]] && return 0
    sleep 0.25
  done
  return 1
}

stop_one() { # name
  local f="$RUN_DIR/$1.pid" pid i
  [[ -f "$f" ]] || return 0
  pid=$(cat "$f")
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    for ((i = 0; i < 80; i++)); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.25
    done
    if kill -0 "$pid" 2>/dev/null; then
      echo "  $1 (pid $pid) ignored SIGTERM for 20 s; sending SIGKILL"
      kill -KILL "$pid" 2>/dev/null || true
    fi
    echo "  stopped $1 (pid $pid)"
  fi
  rm -f "$f"
}
