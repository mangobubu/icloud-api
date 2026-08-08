#!/bin/sh
set -eu
umask 077

state_file="${PGDATA}/.icloud-api-database-credentials"
pgdata_bootstrap_binding_name="pgdata-bootstrap-binding"
temporary="$(mktemp "${state_file}.tmp.XXXXXX")"
umask 077

psql --set=ON_ERROR_STOP=1 --single-transaction \
	--username "$POSTGRES_USER" \
	--dbname "$POSTGRES_DB" \
	--set=app_user="$POSTGRES_APP_USER" \
	--set=app_password="$POSTGRES_APP_PASSWORD" \
	--set=database_name="$POSTGRES_DB" \
	--set=bootstrap_user="$POSTGRES_USER" <<'SQL'
CREATE ROLE :"app_user"
	WITH LOGIN PASSWORD :'app_password'
	NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION;
ALTER DATABASE :"database_name" OWNER TO :"app_user";
ALTER SCHEMA public OWNER TO :"app_user";
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
ALTER ROLE :"bootstrap_user" NOLOGIN;
SQL

printf '%s\n%s\n%s\n%s\n' \
	"$POSTGRES_APP_USER" \
	"$POSTGRES_DB" \
	"$POSTGRES_APP_PASSWORD" \
	"$ICLOUD_API_INSTALLATION_ID" > "$temporary"
chmod 600 "$temporary"
mv -f "$temporary" "$state_file"
sync

config_cluster_state_directory="${ICLOUD_API_DATABASE_CONFIG_DIR:-/run/icloud-api-database}/cluster-state"
config_bootstrap_marker="${config_cluster_state_directory}/allow-cluster-bootstrap"
config_initialized_marker="${config_cluster_state_directory}/cluster-initialized"
config_pgdata_bootstrap_binding="${config_cluster_state_directory}/${pgdata_bootstrap_binding_name}"
mkdir -p "$config_cluster_state_directory"
temporary_config_marker="$(mktemp "${config_initialized_marker}.tmp.XXXXXX")"
printf '%s\n' "$ICLOUD_API_INSTALLATION_ID" > "$temporary_config_marker"
chmod 600 "$temporary_config_marker"
mv -f "$temporary_config_marker" "$config_initialized_marker"

cluster_state_directory="${ICLOUD_API_INSTALLATION_STATE_DIR:-/run/icloud-api-installation}/cluster-state"
bootstrap_marker="${cluster_state_directory}/allow-cluster-bootstrap"
initialized_marker="${cluster_state_directory}/cluster-initialized"
cluster_pgdata_bootstrap_binding="${cluster_state_directory}/${pgdata_bootstrap_binding_name}"
mkdir -p "$cluster_state_directory"
chmod 700 "$cluster_state_directory"
temporary="$(mktemp "${initialized_marker}.tmp.XXXXXX")"
printf '%s\n' "$ICLOUD_API_INSTALLATION_ID" > "$temporary"
chmod 600 "$temporary"
mv -f "$temporary" "$initialized_marker"
sync

rm -f \
	"$config_bootstrap_marker" \
	"$config_pgdata_bootstrap_binding" \
	"$bootstrap_marker" \
	"$cluster_pgdata_bootstrap_binding"
sync
