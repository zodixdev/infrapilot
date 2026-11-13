#!/usr/bin/env bash
# Runs on first start of the local Postgres volume, and again on every `make up` via `make db-provision`
# (idempotent). Implements ADR-0018:
#   - Keycloak database + role.
#   - Per service in INFRAPILOT_SERVICES (comma-separated, [a-z][a-z0-9_]*): database owned by
#     <svc>_migrator, runtime role <svc>_app (NOBYPASSRLS), cross-tenant role <svc>_batch (BYPASSRLS),
#     PUBLIC privileges revoked, server-side timeouts as role defaults.
# Values are passed as psql variables (:'literal', :"identifier"), never spliced into SQL text, and the
# session never logs failing statements (they would contain passwords).
set -euo pipefail

PSQL=(psql -v ON_ERROR_STOP=1 -X -q --username "$POSTGRES_USER")

"${PSQL[@]}" --dbname postgres -v pw="${KEYCLOAK_DB_PASSWORD:?KEYCLOAK_DB_PASSWORD is required}" <<'SQL'
SET log_min_error_statement = panic;
-- Keep ALTER ROLE ... PASSWORD out of pg_stat_statements (readable by pg_read_all_stats and on disk).
SET pg_stat_statements.track_utility = off;
SELECT 'CREATE ROLE keycloak LOGIN' WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keycloak') \gexec
ALTER ROLE keycloak PASSWORD :'pw';
SELECT 'CREATE DATABASE keycloak OWNER keycloak' WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'keycloak') \gexec
REVOKE ALL ON DATABASE keycloak FROM PUBLIC;
GRANT CONNECT ON DATABASE keycloak TO keycloak;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
SQL

IFS=',' read -ra services <<< "${INFRAPILOT_SERVICES:-}"
for svc in "${services[@]}"; do
  svc="$(echo "$svc" | tr -d '[:space:]')"
  [[ -z "$svc" ]] && continue
  if [[ ! "$svc" =~ ^[a-z][a-z0-9_]{0,40}$ ]]; then
    echo "init: invalid service name '$svc' (must match ^[a-z][a-z0-9_]{0,40}\$)" >&2
    exit 1
  fi
  pw="${INFRAPILOT_SERVICE_DB_PASSWORD:?INFRAPILOT_SERVICE_DB_PASSWORD is required when INFRAPILOT_SERVICES is set}"
  echo "init: provisioning database ${svc} (roles ${svc}_migrator, ${svc}_app, ${svc}_batch)"
  "${PSQL[@]}" --dbname postgres -v svc="$svc" -v pw="$pw" <<'SQL'
SET log_min_error_statement = panic;
SET pg_stat_statements.track_utility = off;
\set migrator :svc '_migrator'
\set app :svc '_app'
\set batch :svc '_batch'
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS', :'migrator') WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'migrator') \gexec
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS', :'app') WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'app') \gexec
SELECT format('CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS', :'batch') WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'batch') \gexec
ALTER ROLE :"migrator" PASSWORD :'pw';
ALTER ROLE :"app" PASSWORD :'pw';
ALTER ROLE :"batch" PASSWORD :'pw';
ALTER ROLE :"app" SET statement_timeout = '30s';
ALTER ROLE :"app" SET lock_timeout = '5s';
ALTER ROLE :"app" SET idle_in_transaction_session_timeout = '60s';
ALTER ROLE :"batch" SET statement_timeout = '15min';
ALTER ROLE :"batch" SET idle_in_transaction_session_timeout = '5min';
ALTER ROLE :"migrator" SET lock_timeout = '3s';
SELECT format('CREATE DATABASE %I OWNER %I', :'svc', :'migrator') WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = :'svc') \gexec
REVOKE ALL ON DATABASE :"svc" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"svc" TO :"migrator", :"app", :"batch";
SQL
  "${PSQL[@]}" --dbname "$svc" -v svc="$svc" <<'SQL'
\set migrator :svc '_migrator'
\set app :svc '_app'
\set batch :svc '_batch'
REVOKE ALL ON SCHEMA public FROM PUBLIC;
ALTER SCHEMA public OWNER TO :"migrator";
GRANT USAGE ON SCHEMA public TO :"app", :"batch";
-- Volumes provisioned by earlier versions of this script carry default privileges; migrations now grant per
-- table (ADR-0018), so remove them. Idempotent.
ALTER DEFAULT PRIVILEGES FOR ROLE :"migrator" IN SCHEMA public REVOKE ALL ON TABLES FROM :"app", :"batch";
ALTER DEFAULT PRIVILEGES FOR ROLE :"migrator" IN SCHEMA public REVOKE ALL ON SEQUENCES FROM :"app", :"batch";
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
-- Drop statement texts recorded before track_utility was disabled (they may contain role passwords).
SELECT pg_stat_statements_reset() WHERE EXISTS (SELECT 1 FROM pg_stat_statements WHERE query ILIKE '%PASSWORD%');
SQL
done
