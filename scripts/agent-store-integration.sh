#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/.." && pwd)"
cd "${repo_root}"

container_name="agent-emails-agent-store-${RANDOM}-$$"
postgres_password="agent-store-test"
container_started=0

cleanup() {
  if [[ "${container_started}" == "1" ]]; then
    docker rm -f "${container_name}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

docker run --detach --rm \
  --name "${container_name}" \
  --env POSTGRES_USER=email \
  --env POSTGRES_PASSWORD="${postgres_password}" \
  --env POSTGRES_DB=agent_store_test \
  --publish 127.0.0.1::5432 \
  pgvector/pgvector:pg16 >/dev/null
container_started=1

for _ in $(seq 1 30); do
  if docker exec "${container_name}" pg_isready --username email --dbname agent_store_test >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if ! docker exec "${container_name}" pg_isready --username email --dbname agent_store_test >/dev/null 2>&1; then
  echo "PostgreSQL did not become ready" >&2
  exit 1
fi

published_port="$(docker port "${container_name}" 5432/tcp | sed -E 's/.*:([0-9]+)$/\1/' | tail -n 1)"
if [[ ! "${published_port}" =~ ^[0-9]+$ ]]; then
  echo "Could not determine PostgreSQL port" >&2
  exit 1
fi

AGENT_EMAILS_AGENT_STORE_TEST_DSN="postgres://email:${postgres_password}@127.0.0.1:${published_port}/agent_store_test?sslmode=disable" \
  go test ./internal/store -run '^Test(AgentStoreAliasOwnershipAndDraftDeliveryLifecycle|DraftThreadMembershipFilteringAndConversationStatus|MailboxCredentialEncryptionAndMigration|AtomicMailboxOnboarding)$' -count=1 -v
