#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="${COMPOSE_FILE:-${repo_root}/deploy/compose.yml}"
project_name="${COMPOSE_PROJECT_NAME:-benkz}"
profile="${PROFILE:?set PROFILE, for example water or hot-economy}"
fixture="${FIXTURE:?set FIXTURE, for example /fixtures/hot-write-15000x18.json}"

case "${profile}" in
  water|water-visitor|harvest|sell|hot-economy|steal) ;;
  *)
    echo "unsupported PROFILE=${profile}" >&2
    exit 2
    ;;
esac

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
