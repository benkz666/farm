#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="${COMPOSE_FILE:-${repo_root}/deploy/compose.yml}"
project_name="${COMPOSE_PROJECT_NAME:-benkz}"
profile="${PROFILE:?set PROFILE, for example water or hot-economy}"
fixture="${FIXTURE:?set FIXTURE, for example /fixtures/hot-write-15000x18.json}"

wait_write_journal_idle() {
  if [[ "${WAIT_WRITE_JOURNAL_IDLE:-1}" != "1" ]]; then
    return
  fi

  local timeout_seconds="${WRITE_JOURNAL_IDLE_TIMEOUT_SECONDS:-600}"
  local poll_seconds="${WRITE_JOURNAL_IDLE_POLL_SECONDS:-2}"
  local deadline=$((SECONDS + timeout_seconds))
  local values pending lag

  while ((SECONDS < deadline)); do
    values="$(
      docker compose -p "${project_name}" -f "${compose_file}" \
        --profile app exec -T event-redis sh -lc '
          for key in $(redis-cli --scan --pattern "*:events"); do
            redis-cli --raw XINFO GROUPS "$key"
          done | awk '\''
            $0 == "pending" { getline; pending += $0 }
            $0 == "lag" { getline; lag += $0 }
            END { print pending + 0, lag + 0 }
          '\''
        '
    )"
    read -r pending lag <<<"${values}"
    if [[ "${pending:-1}" == "0" && "${lag:-1}" == "0" ]]; then
      echo "benchfixture: write journal is idle"
      return
    fi
    echo "benchfixture: waiting for write journal pending=${pending:-unknown} lag=${lag:-unknown}"
    sleep "${poll_seconds}"
  done

  echo "write journal did not become idle within ${timeout_seconds}s" >&2
  return 1
}

case "${profile}" in
  water|water-visitor|harvest|sell|hot-economy|steal) ;;
  *)
    echo "unsupported PROFILE=${profile}" >&2
    exit 2
    ;;
esac

# The fixture rewrite bypasses the asynchronous write journal and updates
# MySQL directly. Wait for every older projection first; otherwise a projector
# can commit an earlier benchmark mutation after the reset and make a legal
# plot appear watered/harvested before the next measurement starts.
wait_write_journal_idle

docker compose -p "${project_name}" -f "${compose_file}" \
  --profile app --profile fixture run --rm --no-deps benchfixture \
  -mysql-dsn "${MYSQL_DSN:-farm:farm@tcp(mysql:3306)/farm?parseTime=true&loc=Local}" \
  -redis-addr "${REDIS_ADDR:-redis:6379}" \
  -concurrency "${RESET_CONCURRENCY:-32}" \
  -profile "${profile}" \
  -time-profile "${FARM_TIME_PROFILE:-demo}" \
  -reset-input "${fixture}"

# Reset writes MySQL directly. Restart Farm by default so no resident Actor can
# keep the pre-reset aggregate in memory. Set RESTART_FARM=0 only when Farm is
# already stopped.
if [[ "${RESTART_FARM:-1}" == "1" ]]; then
  docker compose -p "${project_name}" -f "${compose_file}" \
    --profile app restart farm

  # A restart returning only means the container process was started. When a
  # durable journal exists Farm may still be recovering before it opens the
  # admin/business ports. CI or remote runs can provide the published readyz
  # URL; local runs retain a short compatibility settle delay.
  if [[ -n "${FARM_READY_URL:-}" ]]; then
    ready_deadline="${FARM_READY_TIMEOUT_SECONDS:-30}"
    ready=0
    for ((attempt = 0; attempt < ready_deadline; attempt++)); do
      if curl --fail --silent --show-error "${FARM_READY_URL}" >/dev/null; then
        ready=1
        break
      fi
      sleep 1
    done
    if [[ "${ready}" != "1" ]]; then
      echo "Farm did not become ready at ${FARM_READY_URL} within ${ready_deadline}s" >&2
      exit 1
    fi
  else
    sleep "${FARM_RESTART_SETTLE_SECONDS:-2}"
  fi
fi
