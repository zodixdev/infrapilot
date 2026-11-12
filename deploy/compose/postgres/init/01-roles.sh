#!/usr/bin/env bash
# Runs once on first start of the local Postgres volume.
# 1. Keycloak database + role.
# 2. Per-service databases with the three roles of ADR-0018 for every name in INFRAPILOT_SERVICES
#    (comma-separated, e.g. "controlplane,checks"); all local roles share INFRAPILOT_SERVICE_DB_PASSWORD.
set -euo pipefail

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
CREATE ROLE keycloak LOGIN PASSWORD '${KEYCLOAK_DB_PASSWORD}';
CREATE DATABASE keycloak OWNER keycloak;
SQL

IFS=',' read -ra services <<< "${INFRAPILOT_SERVICES:-}"
for svc in "${services[@]}"; do
  svc="$(echo "$svc" | tr -d '[:space:]')"
  [[ -z "$svc" ]] && continue
  pw="${INFRAPILOT_SERVICE_DB_PASSWORD:?INFRAPILOT_SERVICE_DB_PASSWORD is required when INFRAPILOT_SERVICES is set}"
  echo "init: provisioning database ${svc} with roles ${svc}_migrator / ${svc}_app / ${svc}_batch"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
CREATE ROLE ${svc}_migrator LOGIN PASSWORD '${pw}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
CREATE ROLE ${svc}_app      LOGIN PASSWORD '${pw}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
CREATE ROLE ${svc}_batch    LOGIN PASSWORD '${pw}' NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS;
CREATE DATABASE ${svc} OWNER ${svc}_migrator;
SQL
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$svc" <<SQL
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO ${svc}_migrator;
GRANT USAGE ON SCHEMA public TO ${svc}_app, ${svc}_batch;
ALTER DEFAULT PRIVILEGES FOR ROLE ${svc}_migrator IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ${svc}_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ${svc}_migrator IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO ${svc}_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ${svc}_migrator IN SCHEMA public GRANT SELECT ON TABLES TO ${svc}_batch;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
done
