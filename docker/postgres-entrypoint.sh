#!/bin/sh
set -eu
umask 077

die() {
	printf '%s\n' "PostgreSQL 启动失败：$*" >&2
	exit 1
}

random_hex() {
	random_hex_byte_count="$1"
	random_hex_value="$(od -An -N "$random_hex_byte_count" -tx1 /dev/urandom | tr -d ' \n')"
	[ "${#random_hex_value}" -eq "$((random_hex_byte_count * 2))" ] || die "生成数据库凭据失败"
	printf '%s' "$random_hex_value"
}

load_credentials() {
	load_credentials_file="$1"
	[ -f "$load_credentials_file" ] || die "凭据文件不存在：$load_credentials_file"
	[ "$(wc -l < "$load_credentials_file" | tr -d ' ')" = "4" ] || \
		die "凭据文件格式错误：$load_credentials_file"
	LOADED_USER="$(sed -n '1p' "$load_credentials_file")"
	LOADED_DATABASE="$(sed -n '2p' "$load_credentials_file")"
	LOADED_PASSWORD="$(sed -n '3p' "$load_credentials_file")"
	LOADED_INSTALLATION_ID="$(sed -n '4p' "$load_credentials_file")"
	case "$LOADED_USER" in
		[a-z][a-z0-9_]*) ;;
		*) die "数据库用户名格式错误：$load_credentials_file" ;;
	esac
	case "$LOADED_USER" in
		*[!a-z0-9_]*) die "数据库用户名格式错误：$load_credentials_file" ;;
	esac
	case "$LOADED_DATABASE" in
		[a-z][a-z0-9_]*) ;;
		*) die "数据库名格式错误：$load_credentials_file" ;;
	esac
	case "$LOADED_DATABASE" in
		*[!a-z0-9_]*) die "数据库名格式错误：$load_credentials_file" ;;
	esac
	[ "${#LOADED_PASSWORD}" -eq 64 ] || die "数据库密码格式错误：$load_credentials_file"
	case "$LOADED_PASSWORD" in
		*[!0-9a-f]*) die "数据库密码格式错误：$load_credentials_file" ;;
	esac
	[ "${#LOADED_INSTALLATION_ID}" -eq 32 ] || die "安装标识格式错误：$load_credentials_file"
	case "$LOADED_INSTALLATION_ID" in
		*[!0-9a-f]*) die "安装标识格式错误：$load_credentials_file" ;;
	esac
	load_credentials_expected_bytes="$((
		${#LOADED_USER} +
		${#LOADED_DATABASE} +
		${#LOADED_PASSWORD} +
		${#LOADED_INSTALLATION_ID} +
		4
	))"
	[ "$(wc -c < "$load_credentials_file" | tr -d ' ')" = "$load_credentials_expected_bytes" ] || \
		die "凭据文件包含尾部数据或格式错误：$load_credentials_file"
}

validate_bound_marker() {
	validate_marker_path="$1"
	validate_marker_expected_value="$2"
	validate_marker_description="$3"
	[ -f "$validate_marker_path" ] && [ ! -L "$validate_marker_path" ] || \
		die "$validate_marker_description 不存在或不是普通文件"
	[ "$(stat -c '%h' "$validate_marker_path")" = "1" ] || \
		die "$validate_marker_description 不能是硬链接"
	[ "$(wc -l < "$validate_marker_path" | tr -d ' ')" = "1" ] || \
		die "$validate_marker_description 格式错误"
	[ "$(wc -c < "$validate_marker_path" | tr -d ' ')" -eq \
		"$(( ${#validate_marker_expected_value} + 1 ))" ] || \
		die "$validate_marker_description 格式错误"
	[ "$(sed -n '1p' "$validate_marker_path")" = "$validate_marker_expected_value" ] || \
		die "$validate_marker_description 与当前安装不匹配"
}

write_shared_credentials() {
	write_credentials_user="$1"
	write_credentials_database="$2"
	write_credentials_password="$3"
	write_credentials_installation_id="$4"
	mkdir -p "$config_directory"
	chown 0:0 "$config_directory"
	chmod 755 "$config_directory"
	write_credentials_temporary="$(mktemp "${credentials_file}.tmp.XXXXXX")"
	umask 077
	printf '%s\n%s\n%s\n%s\n' \
		"$write_credentials_user" \
		"$write_credentials_database" \
		"$write_credentials_password" \
		"$write_credentials_installation_id" > "$write_credentials_temporary"
	chown 0:10001 "$write_credentials_temporary"
	chmod 640 "$write_credentials_temporary"
	sync || die "同步数据库配置凭据临时文件失败"
	mv -f "$write_credentials_temporary" "$credentials_file"
}

write_pgdata_credentials() {
	write_pgdata_user="$1"
	write_pgdata_database="$2"
	write_pgdata_password="$3"
	write_pgdata_installation_id="$4"
	write_pgdata_temporary="$(mktemp "${state_file}.tmp.XXXXXX")"
	umask 077
	printf '%s\n%s\n%s\n%s\n' \
		"$write_pgdata_user" \
		"$write_pgdata_database" \
		"$write_pgdata_password" \
		"$write_pgdata_installation_id" > "$write_pgdata_temporary"
	chown 70:70 "$write_pgdata_temporary"
	chmod 600 "$write_pgdata_temporary"
	sync || die "同步 PGDATA 凭据状态临时文件失败"
	mv -f "$write_pgdata_temporary" "$state_file"
}

write_key_marker() {
	write_key_marker_path="$1"
	write_key_marker_value="$2"
	mkdir -p "$app_state_directory"
	chown 10001:10001 "$app_state_directory"
	chmod 700 "$app_state_directory"
	write_key_marker_temporary="$(mktemp "${write_key_marker_path}.tmp.XXXXXX")"
	umask 077
	printf '%s\n' "$write_key_marker_value" > "$write_key_marker_temporary"
	chown 10001:10001 "$write_key_marker_temporary"
	chmod 600 "$write_key_marker_temporary"
	sync || die "同步主密钥状态临时文件失败"
	mv -f "$write_key_marker_temporary" "$write_key_marker_path"
}

write_cluster_marker() {
	write_cluster_marker_path="$1"
	write_cluster_marker_value="$2"
	write_cluster_marker_directory="${write_cluster_marker_path%/*}"
	mkdir -p "$write_cluster_marker_directory"
	chown 70:70 "$write_cluster_marker_directory"
	chmod 700 "$write_cluster_marker_directory"
	write_cluster_marker_temporary="$(mktemp "${write_cluster_marker_path}.tmp.XXXXXX")"
	umask 077
	printf '%s\n' "$write_cluster_marker_value" > "$write_cluster_marker_temporary"
	chown 70:70 "$write_cluster_marker_temporary"
	chmod 600 "$write_cluster_marker_temporary"
	sync || die "同步数据库状态临时文件失败"
	mv -f "$write_cluster_marker_temporary" "$write_cluster_marker_path"
}

repair_legacy_pgdata_binding_state() {
	repair_user="$1"
	repair_database="$2"
	repair_password="$3"
	repair_installation_id="$4"
	repair_pgdata_binding="$5"

	for repair_directory in \
		"$config_directory" \
		"$config_cluster_state_directory" \
		"$installation_state_directory" \
		"$cluster_state_directory" \
		"$app_state_directory" \
		"$postgres_data"; do
		[ -d "$repair_directory" ] && [ ! -L "$repair_directory" ] || \
			die "旧版安装标识修复要求状态目录不是软链接"
	done
	[ -f "$credentials_file" ] && [ ! -L "$credentials_file" ] || \
		die "旧版安装标识修复要求数据库配置凭据为普通文件"
	[ "$(stat -c '%h' "$credentials_file")" = "1" ] || \
		die "旧版安装标识修复拒绝硬链接数据库配置凭据"
	[ "$(stat -c '%u:%g:%a' "$credentials_file")" = "0:10001:640" ] || \
		die "旧版安装标识修复要求数据库配置凭据权限为 0:10001/0640"
	[ -f "$state_file" ] && [ ! -L "$state_file" ] || \
		die "旧版安装标识修复要求 PGDATA 凭据状态为普通文件"
	[ "$(stat -c '%h' "$state_file")" = "1" ] || \
		die "旧版安装标识修复拒绝硬链接 PGDATA 凭据状态"
	[ "$(stat -c '%u:%g:%a' "$state_file")" = "70:70:600" ] || \
		die "旧版安装标识修复要求 PGDATA 凭据状态权限为 70:70/0600"
	[ -f "${postgres_data}/PG_VERSION" ] && [ ! -L "${postgres_data}/PG_VERSION" ] || \
		die "旧版安装标识修复要求 PG_VERSION 为普通文件"
	[ -s "${postgres_data}/PG_VERSION" ] || \
		die "旧版安装标识修复要求 PG_VERSION 非空"
	[ "$(stat -c '%h' "${postgres_data}/PG_VERSION")" = "1" ] || \
		die "旧版安装标识修复拒绝硬链接 PG_VERSION"
	[ ! -e "${postgres_data}/postmaster.pid" ] && [ ! -L "${postgres_data}/postmaster.pid" ] || \
		die "旧版安装标识修复要求 PostgreSQL 已完全停止"
	[ "$(wc -l < "$state_file" | tr -d ' ')" = "4" ] || \
		die "旧版安装标识修复要求 PGDATA 凭据状态恰好四行"
	[ "$(sed -n '1p' "$state_file")" = "$repair_user" ] && \
		[ "$(sed -n '2p' "$state_file")" = "$repair_database" ] && \
		[ "$(sed -n '3p' "$state_file")" = "$repair_password" ] && \
		[ "$(sed -n '4p' "$state_file")" = "$repair_pgdata_binding" ] || \
		die "旧版安装标识修复拒绝不匹配的 PGDATA 凭据状态"
	repair_state_expected_bytes="$((
		${#repair_user} +
		${#repair_database} +
		${#repair_password} +
		${#repair_pgdata_binding} +
		4
	))"
	[ "$(wc -c < "$state_file" | tr -d ' ')" = "$repair_state_expected_bytes" ] || \
		die "旧版安装标识修复拒绝带尾部数据的 PGDATA 凭据状态"

	for repair_cluster_marker in \
		"$config_cluster_initialized_marker" "$cluster_initialized_marker"; do
		[ -f "$repair_cluster_marker" ] && [ ! -L "$repair_cluster_marker" ] || \
			die "旧版安装标识修复要求数据库完成标记为普通文件"
		repair_cluster_marker_value="$(sed -n '1p' "$repair_cluster_marker")"
		case "$repair_cluster_marker_value" in
			"$repair_pgdata_binding"|"$repair_installation_id") ;;
			*) die "旧版安装标识修复拒绝不匹配的数据库完成标记" ;;
		esac
		validate_bound_marker "$repair_cluster_marker" "$repair_cluster_marker_value" \
			"旧版数据库完成标记"
		[ "$(stat -c '%u:%g:%a' "$repair_cluster_marker")" = "70:70:600" ] || \
			die "旧版安装标识修复要求数据库完成标记权限为 70:70/0600"
	done

	repair_app_marker_count=0
	for repair_app_marker in "$bootstrap_marker" "$initialized_marker"; do
		if [ -e "$repair_app_marker" ] || [ -L "$repair_app_marker" ]; then
			validate_bound_marker "$repair_app_marker" "$repair_installation_id" \
				"旧版应用主密钥状态标记"
			[ "$(stat -c '%u:%g:%a' "$repair_app_marker")" = "10001:10001:600" ] || \
				die "旧版安装标识修复要求应用状态标记权限为 10001:10001/0600"
			repair_app_marker_count="$((repair_app_marker_count + 1))"
		fi
	done
	[ "$repair_app_marker_count" -eq 1 ] || \
		die "旧版安装标识修复要求恰好一个应用主密钥状态标记"

	for repair_stale_marker in \
		"$config_cluster_bootstrap_marker" \
		"$config_pgdata_bootstrap_binding" \
		"$cluster_bootstrap_marker" \
		"$cluster_pgdata_bootstrap_binding"; do
		[ ! -e "$repair_stale_marker" ] && [ ! -L "$repair_stale_marker" ] || \
			die "旧版安装标识修复拒绝仍含首次初始化标记的状态"
	done

	# Publish and sync the markers first. If interrupted, the still-legacy
	# PGDATA credential keeps this exact repair signature discoverable.
	write_cluster_marker "$cluster_initialized_marker" "$repair_installation_id"
	write_cluster_marker "$config_cluster_initialized_marker" "$repair_installation_id"
	sync || die "持久化旧版安装标识修复标记失败"
	write_pgdata_credentials \
		"$repair_user" "$repair_database" "$repair_password" "$repair_installation_id"
	sync || die "持久化旧版安装标识修复失败"
	printf '%s\n' "PostgreSQL：已修复旧版 PGDATA 绑定变量污染的安装标识。" >&2
}

config_directory="${ICLOUD_API_DATABASE_CONFIG_DIR:-/run/icloud-api-database}"
credentials_file="${config_directory}/credentials"
config_cluster_state_directory="${config_directory}/cluster-state"
config_cluster_bootstrap_marker="${config_cluster_state_directory}/allow-cluster-bootstrap"
config_cluster_initialized_marker="${config_cluster_state_directory}/cluster-initialized"
config_pgdata_bootstrap_binding="${config_cluster_state_directory}/pgdata-bootstrap-binding"
installation_state_directory="${ICLOUD_API_INSTALLATION_STATE_DIR:-/run/icloud-api-installation}"
app_state_directory="${installation_state_directory}/app-state"
bootstrap_marker="${app_state_directory}/allow-key-bootstrap"
initialized_marker="${app_state_directory}/key-initialized"
cluster_state_directory="${installation_state_directory}/cluster-state"
cluster_bootstrap_marker="${cluster_state_directory}/allow-cluster-bootstrap"
cluster_initialized_marker="${cluster_state_directory}/cluster-initialized"
cluster_pgdata_bootstrap_binding="${cluster_state_directory}/pgdata-bootstrap-binding"
postgres_data="${PGDATA:-/var/lib/postgresql/data}"
state_file="${postgres_data}/.icloud-api-database-credentials"

acquire_backup_restore_lock() {
	mkdir -p "$installation_state_directory" || die "创建安装状态目录失败"
	chmod 755 "$installation_state_directory" || die "设置安装状态目录权限失败"
	exec 8>"${installation_state_directory}/restore.lock"
	flock -n 8 || die "另一个 PostgreSQL 备份或恢复任务正在运行"
}

case "${1:-}" in
	maintenance-lock)
		shift
		[ "$#" -eq 1 ] || die "maintenance-lock 需要一个 32 位十六进制就绪令牌"
		maintenance_token="$1"
		[ "${#maintenance_token}" -eq 32 ] || die "maintenance-lock 就绪令牌格式错误"
		case "$maintenance_token" in
			*[!0-9a-f]*) die "maintenance-lock 就绪令牌格式错误" ;;
		esac
		[ -d "$installation_state_directory" ] && [ ! -L "$installation_state_directory" ] || \
			die "安装状态卷不存在或不是普通目录"
		mkdir -p "$app_state_directory" || die "创建应用状态目录失败"
		[ -d "$app_state_directory" ] && [ ! -L "$app_state_directory" ] || \
			die "应用状态目录不是普通目录"
		chown 10001:10001 "$app_state_directory" || die "设置应用状态目录所有者失败"
		chmod 700 "$app_state_directory" || die "设置应用状态目录权限失败"
		maintenance_lock_file="${app_state_directory}/maintenance.lock"
		maintenance_window_lock_file="${app_state_directory}/maintenance-window.lock"
		maintenance_ready_file="${app_state_directory}/maintenance-window.ready"
		for lock_file in "$maintenance_lock_file" "$maintenance_window_lock_file"; do
			if [ -e "$lock_file" ] || [ -L "$lock_file" ]; then
				[ -f "$lock_file" ] && [ ! -L "$lock_file" ] || die "维护锁不是普通文件"
				[ "$(stat -c '%h' "$lock_file")" = "1" ] || die "维护锁不能是硬链接"
			else
				: > "$lock_file" || die "创建维护锁失败"
			fi
			chown 10001:10001 "$lock_file" || die "设置维护锁所有者失败"
			chmod 600 "$lock_file" || die "设置维护锁权限失败"
		done
		exec 8>"$maintenance_lock_file" || die "打开共享维护锁失败"
		flock -n 8 || die "另一个管理员重置、备份或恢复任务正在运行"
		exec 7>"$maintenance_window_lock_file" || die "打开维护窗口上下文锁失败"
		flock -n 7 || die "另一个备份或恢复窗口正在运行"
		maintenance_ready_temporary=""
		cleanup_maintenance_lock() {
			status="$?"
			trap - EXIT HUP INT TERM
			if [ -n "$maintenance_ready_temporary" ]; then
				rm -f "$maintenance_ready_temporary" || status=1
			fi
			if [ -f "$maintenance_ready_file" ] && [ ! -L "$maintenance_ready_file" ] && \
				[ "$(sed -n '1p' "$maintenance_ready_file")" = "$maintenance_token" ]; then
				rm -f "$maintenance_ready_file" || status=1
			fi
			exit "$status"
		}
		trap cleanup_maintenance_lock EXIT
		trap 'exit 0' HUP INT TERM
		if [ -e "$maintenance_ready_file" ] || [ -L "$maintenance_ready_file" ]; then
			[ -f "$maintenance_ready_file" ] && [ ! -L "$maintenance_ready_file" ] || \
				die "维护窗口就绪标记不是普通文件"
		fi
		maintenance_ready_temporary="$(mktemp "${maintenance_ready_file}.tmp.XXXXXX")" || \
			die "创建维护窗口就绪标记失败"
		printf '%s\n' "$maintenance_token" > "$maintenance_ready_temporary" || \
			die "写入维护窗口就绪标记失败"
		chown 10001:10001 "$maintenance_ready_temporary" || die "设置维护窗口就绪标记所有者失败"
		chmod 600 "$maintenance_ready_temporary" || die "设置维护窗口就绪标记权限失败"
		mv -f "$maintenance_ready_temporary" "$maintenance_ready_file" || \
			die "发布维护窗口就绪标记失败"
		maintenance_ready_temporary=""
		while :; do
			sleep 1
		done
		;;
	healthcheck)
		load_credentials "$credentials_file"
		[ -f "$cluster_initialized_marker" ] || exit 1
		[ "$(sed -n '1p' "$cluster_initialized_marker")" = "$LOADED_INSTALLATION_ID" ] || exit 1
		[ -f "${postgres_data}/postmaster.pid" ] || exit 1
		[ "$(sed -n '1p' "${postgres_data}/postmaster.pid" | tr -d ' ')" = "1" ] || exit 1
		PGPASSWORD="$LOADED_PASSWORD"
		export PGPASSWORD
		exec psql -h /var/run/postgresql -U "$LOADED_USER" -d "$LOADED_DATABASE" -Atqc "SELECT 1"
		;;
	backup)
		shift
		acquire_backup_restore_lock
		load_credentials "$credentials_file"
		PGPASSWORD="$LOADED_PASSWORD"
		export PGPASSWORD
		[ "$#" -eq 0 ] || die "backup 不接受改变 pg_dump 范围或格式的额外参数"
		exec pg_dump -h /var/run/postgresql -U "$LOADED_USER" -d "$LOADED_DATABASE" \
			--format=custom --no-owner --no-acl
		;;
	restore)
		shift
		acquire_backup_restore_lock
		load_credentials "$credentials_file"
		PGPASSWORD="$LOADED_PASSWORD"
		export PGPASSWORD
		[ "$#" -le 1 ] || die "restore 只接受一个 custom archive 路径，或从 stdin 读取归档"
		restore_archive=""
		restore_toc=""
		restore_sql=""
		restore_batch=""
		cleanup_restore_files() {
			for restore_temporary in \
				"$restore_archive" "$restore_toc" "$restore_sql" "$restore_batch"; do
				[ -z "$restore_temporary" ] || rm -f "$restore_temporary"
			done
		}
		trap cleanup_restore_files EXIT
		trap 'exit 1' HUP INT TERM
		restore_archive="$(mktemp "${TMPDIR:-/tmp}/icloud-api-restore.XXXXXX")" || \
			die "创建恢复归档临时文件失败"
		restore_toc="$(mktemp "${TMPDIR:-/tmp}/icloud-api-restore-toc.XXXXXX")" || \
			die "创建恢复目录临时文件失败"
		restore_sql="$(mktemp "${TMPDIR:-/tmp}/icloud-api-restore-sql.XXXXXX")" || \
			die "创建恢复 SQL 临时文件失败"
		restore_batch="$(mktemp "${TMPDIR:-/tmp}/icloud-api-restore-batch.XXXXXX")" || \
			die "创建恢复事务临时文件失败"

		if [ "$#" -eq 0 ] || [ "$1" = "-" ]; then
			cat > "$restore_archive" || die "读取 stdin 中的恢复归档失败"
		else
			cat < "$1" > "$restore_archive" || die "读取恢复归档失败：$1"
		fi
		[ -s "$restore_archive" ] || die "恢复归档为空"
		restore_magic="$(od -An -N 5 -tx1 "$restore_archive" | tr -d ' \n')"
		[ "$restore_magic" = "5047444d50" ] || die "恢复输入不是 pg_dump custom archive"
		if ! pg_restore --list "$restore_archive" > "$restore_toc"; then
			die "恢复输入不是有效的 pg_dump custom archive"
		fi
		if ! awk '
			/^[[:space:]]*;/ || /^[[:space:]]*$/ { next }
			{ found = 1 }
			END { exit(found ? 0 : 1) }
		' "$restore_toc"; then
			die "恢复归档不包含任何数据库对象"
		fi
		auto_alias_table_count=0
		for auto_alias_table in alias_creation_schedules pending_alias_api_keys; do
			if awk -v required_table="$auto_alias_table" '
				$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
					$6 == required_table { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				auto_alias_table_count=$((auto_alias_table_count + 1))
			fi
		done
		seen_table_count=0
		for seen_table in consumed_messages imap_seen_tasks; do
			if awk -v required_table="$seen_table" '
				$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
					$6 == required_table { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				seen_table_count=$((seen_table_count + 1))
			fi
		done
		archive_table_count=0
		for archive_table in archived_messages alias_messages; do
			if awk -v required_table="$archive_table" '
				$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
					$6 == required_table { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				archive_table_count=$((archive_table_count + 1))
			fi
		done
		latest_message_table_count=0
		if awk '
			$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
				$6 == "latest_messages" { found = 1 }
			END { exit(found ? 0 : 1) }
		' "$restore_toc"; then
			latest_message_table_count=1
		fi
		mailbox_required_tables=""
		mailbox_required_constraints=""
		mailbox_required_foreign_keys=""
		mailbox_required_indexes=""
		if awk '
			$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
				$6 == "account_mailbox_settings" { found = 1 }
			END { exit(found ? 0 : 1) }
		' "$restore_toc"; then
			mailbox_required_tables="account_mailbox_settings"
			mailbox_required_constraints="account_mailbox_settings:account_mailbox_settings_pkey"
			mailbox_required_foreign_keys="account_mailbox_settings:account_mailbox_settings_account_id_fkey"
			mailbox_required_indexes="account_mailbox_settings_type_idx account_mailbox_settings_custom_suffix_idx"
		fi
		optional_required_tables=""
		optional_required_constraints=""
		optional_required_foreign_keys=""
		optional_required_indexes=""
		optional_required_sequences=""
		message_required_tables="latest_messages"
		message_required_constraints="latest_messages:latest_messages_pkey"
		message_required_foreign_keys="latest_messages:latest_messages_alias_id_fkey"
		case "$archive_table_count" in
			0)
				case "$auto_alias_table_count" in
					0|2) ;;
					*) die "恢复归档中的自动别名 schema 表不完整" ;;
				esac
				case "$seen_table_count" in
					0|2) ;;
					*) die "恢复归档中的消费与 IMAP Seen schema 表不完整" ;;
				esac
				if [ "$auto_alias_table_count" -eq 2 ]; then
					optional_required_tables="alias_creation_schedules pending_alias_api_keys"
					optional_required_constraints="alias_creation_schedules:alias_creation_schedules_pkey pending_alias_api_keys:pending_alias_api_keys_pkey"
					optional_required_foreign_keys="alias_creation_schedules:alias_creation_schedules_account_id_fkey pending_alias_api_keys:pending_alias_api_keys_alias_id_fkey"
					optional_required_indexes="alias_creation_schedules_due_idx"
				fi
				if [ "$seen_table_count" -eq 2 ]; then
					optional_required_tables="$optional_required_tables consumed_messages imap_seen_tasks"
					optional_required_constraints="$optional_required_constraints consumed_messages:consumed_messages_pkey imap_seen_tasks:imap_seen_tasks_pkey"
					optional_required_foreign_keys="$optional_required_foreign_keys consumed_messages:consumed_messages_alias_id_fkey imap_seen_tasks:imap_seen_tasks_account_id_fkey"
					optional_required_indexes="$optional_required_indexes imap_seen_tasks_account_created_idx"
				fi
				;;
			2)
				# The first published v7 removed the legacy compatibility tables; application
				# convergence recreates them after restore. Current v7 keeps the complete groups.
				case "$auto_alias_table_count:$seen_table_count:$latest_message_table_count" in
					1:0:0)
						message_required_tables=""
						message_required_constraints=""
						message_required_foreign_keys=""
						optional_required_tables="alias_creation_schedules archived_messages alias_messages"
						optional_required_constraints="alias_creation_schedules:alias_creation_schedules_pkey archived_messages:archived_messages_pkey archived_messages:archived_messages_account_id_uid_validity_upstream_uid_key alias_messages:alias_messages_pkey alias_messages:alias_messages_alias_id_mailbox_uid_key"
						optional_required_foreign_keys="alias_creation_schedules:alias_creation_schedules_account_id_fkey archived_messages:archived_messages_account_id_fkey alias_messages:alias_messages_alias_id_fkey alias_messages:alias_messages_message_id_fkey"
						optional_required_indexes="alias_creation_schedules_due_idx aliases_oauth_client_id_idx aliases_imap_password_hash_idx archived_messages_retention_idx alias_messages_alias_uid_idx alias_messages_alias_otp_idx"
						;;
					2:2:1)
						optional_required_tables="alias_creation_schedules pending_alias_api_keys consumed_messages imap_seen_tasks archived_messages alias_messages"
						optional_required_constraints="alias_creation_schedules:alias_creation_schedules_pkey pending_alias_api_keys:pending_alias_api_keys_pkey consumed_messages:consumed_messages_pkey imap_seen_tasks:imap_seen_tasks_pkey archived_messages:archived_messages_pkey archived_messages:archived_messages_account_id_uid_validity_upstream_uid_key alias_messages:alias_messages_pkey alias_messages:alias_messages_alias_id_mailbox_uid_key"
						optional_required_foreign_keys="alias_creation_schedules:alias_creation_schedules_account_id_fkey pending_alias_api_keys:pending_alias_api_keys_alias_id_fkey consumed_messages:consumed_messages_alias_id_fkey imap_seen_tasks:imap_seen_tasks_account_id_fkey archived_messages:archived_messages_account_id_fkey alias_messages:alias_messages_alias_id_fkey alias_messages:alias_messages_message_id_fkey"
						optional_required_indexes="alias_creation_schedules_due_idx imap_seen_tasks_account_created_idx aliases_oauth_client_id_idx aliases_imap_password_hash_idx archived_messages_retention_idx alias_messages_alias_uid_idx alias_messages_alias_otp_idx"
						;;
					*) die "恢复归档中的 v7 兼容 schema 组合不完整" ;;
				esac
				optional_required_sequences="archived_messages_id_seq"
				;;
			*)
				die "恢复归档中的邮件归档 schema 表不完整"
				;;
		esac
		for required_table in \
			schema_migrations app_metadata admins admin_sessions accounts aliases \
			$message_required_tables audit_logs apple_web_sessions imap_sync_states data_migrations \
			$optional_required_tables $mailbox_required_tables; do
			if ! awk -v required_table="$required_table" '
				$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "public" && \
					$6 == required_table { found_table = 1 }
				$1 ~ /^[0-9]+;$/ && $4 == "TABLE" && $5 == "DATA" && \
					$6 == "public" && $7 == required_table { found_data = 1 }
				END { exit(found_table && found_data ? 0 : 1) }
			' "$restore_toc"; then
				die "恢复归档缺少项目必需表或数据段：public.$required_table"
			fi
		done
		for required_sequence in admins_id_seq accounts_id_seq aliases_id_seq audit_logs_id_seq \
			$optional_required_sequences; do
			if ! awk -v required_sequence="$required_sequence" '
				$1 ~ /^[0-9]+;$/ && $4 == "SEQUENCE" && $5 == "public" && \
					$6 == required_sequence { found_sequence = 1 }
				$1 ~ /^[0-9]+;$/ && $4 == "SEQUENCE" && $5 == "SET" && \
					$6 == "public" && $7 == required_sequence { found_set = 1 }
				END { exit(found_sequence && found_set ? 0 : 1) }
			' "$restore_toc"; then
				die "恢复归档缺少项目必需序列或状态：public.$required_sequence"
			fi
		done
		for required_constraint in \
			schema_migrations:schema_migrations_pkey \
			app_metadata:app_metadata_pkey \
			admins:admins_pkey admins:admins_username_key \
			admin_sessions:admin_sessions_pkey \
			accounts:accounts_pkey accounts:accounts_email_key \
			aliases:aliases_pkey aliases:aliases_address_key aliases:aliases_api_key_hash_key \
			$message_required_constraints audit_logs:audit_logs_pkey \
			apple_web_sessions:apple_web_sessions_pkey \
			imap_sync_states:imap_sync_states_pkey data_migrations:data_migrations_pkey \
			$optional_required_constraints $mailbox_required_constraints; do
			required_constraint_table="${required_constraint%%:*}"
			required_constraint_name="${required_constraint#*:}"
			if ! awk \
				-v required_table="$required_constraint_table" \
				-v required_constraint="$required_constraint_name" '
				$1 ~ /^[0-9]+;$/ && $4 == "CONSTRAINT" && $5 == "public" && \
					$6 == required_table && $7 == required_constraint { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				die "恢复归档缺少项目必需主键或唯一约束：public.$required_constraint_name"
			fi
		done
		for required_foreign_key in \
			admin_sessions:admin_sessions_admin_id_fkey \
			aliases:aliases_account_id_fkey \
			$message_required_foreign_keys \
			audit_logs:audit_logs_admin_id_fkey \
			apple_web_sessions:apple_web_sessions_account_id_fkey \
			imap_sync_states:imap_sync_states_account_id_fkey \
			$optional_required_foreign_keys $mailbox_required_foreign_keys; do
			required_foreign_key_table="${required_foreign_key%%:*}"
			required_foreign_key_name="${required_foreign_key#*:}"
			if ! awk \
				-v required_table="$required_foreign_key_table" \
				-v required_constraint="$required_foreign_key_name" '
				$1 ~ /^[0-9]+;$/ && $4 == "FK" && $5 == "CONSTRAINT" && \
					$6 == "public" && $7 == required_table && \
					$8 == required_constraint { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				die "恢复归档缺少项目必需外键：public.$required_foreign_key_name"
			fi
		done
		for required_index in \
			admin_sessions_expires_at_idx admin_sessions_admin_id_idx \
			accounts_enabled_email_idx aliases_account_id_idx \
			aliases_account_address_idx aliases_enabled_account_address_idx \
			audit_logs_created_at_idx audit_logs_admin_id_idx \
			$optional_required_indexes $mailbox_required_indexes; do
			if ! awk -v required_index="$required_index" '
				$1 ~ /^[0-9]+;$/ && $4 == "INDEX" && $5 == "public" && \
					$6 == required_index { found = 1 }
				END { exit(found ? 0 : 1) }
			' "$restore_toc"; then
				die "恢复归档缺少项目关键索引：public.$required_index"
			fi
		done
		if ! pg_restore \
			--clean --if-exists --no-owner --no-acl --exit-on-error \
			--file="$restore_sql" "$restore_archive"; then
			die "展开 PostgreSQL 恢复归档失败"
		fi
		[ -s "$restore_sql" ] || die "恢复归档没有生成 SQL"
		{
			printf '%s\n' \
				'DROP SCHEMA IF EXISTS public CASCADE;' \
				'CREATE SCHEMA public AUTHORIZATION CURRENT_USER;' \
				'REVOKE CREATE ON SCHEMA public FROM PUBLIC;'
			cat "$restore_sql"
			cat <<'SQL'
DO $icloud_api_restore_validation$
DECLARE
	missing_object TEXT;
	restored_schema_version INTEGER;
	auto_alias_table_count INTEGER;
	seen_table_count INTEGER;
	archive_table_count INTEGER;
BEGIN
	SELECT version
	INTO restored_schema_version
	FROM public.schema_migrations
	WHERE id = 1;
	IF restored_schema_version IS NULL OR restored_schema_version NOT IN (4, 5, 6, 7, 8) THEN
		RAISE EXCEPTION 'restored schema version % is not supported', restored_schema_version;
	END IF;

	SELECT count(*)
	INTO auto_alias_table_count
	FROM (VALUES ('alias_creation_schedules'), ('pending_alias_api_keys')) AS extension(table_name)
	WHERE pg_catalog.to_regclass('public.' || extension.table_name) IS NOT NULL;
	SELECT count(*)
	INTO seen_table_count
	FROM (VALUES ('consumed_messages'), ('imap_seen_tasks')) AS extension(table_name)
	WHERE pg_catalog.to_regclass('public.' || extension.table_name) IS NOT NULL;
	SELECT count(*)
	INTO archive_table_count
	FROM (VALUES ('archived_messages'), ('alias_messages')) AS extension(table_name)
	WHERE pg_catalog.to_regclass('public.' || extension.table_name) IS NOT NULL;
	IF restored_schema_version = 4 AND
		(auto_alias_table_count <> 0 OR seen_table_count <> 0 OR archive_table_count <> 0) THEN
		RAISE EXCEPTION 'restored schema v4 contains later-version objects';
	END IF;
	IF restored_schema_version = 5 AND
		(auto_alias_table_count NOT IN (0, 2) OR seen_table_count NOT IN (0, 2) OR
		 archive_table_count <> 0 OR
		 (auto_alias_table_count = 0 AND seen_table_count = 0)) THEN
		RAISE EXCEPTION 'restored schema v5 contains neither recognized v5 table group';
	END IF;
	IF restored_schema_version = 6 AND
		(auto_alias_table_count <> 2 OR seen_table_count <> 2 OR archive_table_count <> 0) THEN
		RAISE EXCEPTION 'restored schema v6 is missing required table groups';
	END IF;
	IF restored_schema_version BETWEEN 4 AND 6 AND
		pg_catalog.to_regclass('public.latest_messages') IS NULL THEN
		RAISE EXCEPTION 'restored legacy schema is missing latest_messages';
	END IF;
	-- Accept only the original destructive v7 or the current compatibility-preserving v7.
	IF restored_schema_version = 7 AND NOT (
		(
			auto_alias_table_count = 1 AND seen_table_count = 0 AND archive_table_count = 2 AND
			pg_catalog.to_regclass('public.alias_creation_schedules') IS NOT NULL AND
			pg_catalog.to_regclass('public.pending_alias_api_keys') IS NULL AND
			pg_catalog.to_regclass('public.latest_messages') IS NULL AND
			pg_catalog.to_regclass('public.consumed_messages') IS NULL AND
			pg_catalog.to_regclass('public.imap_seen_tasks') IS NULL
		) OR (
			auto_alias_table_count = 2 AND seen_table_count = 2 AND archive_table_count = 2 AND
			pg_catalog.to_regclass('public.alias_creation_schedules') IS NOT NULL AND
			pg_catalog.to_regclass('public.pending_alias_api_keys') IS NOT NULL AND
			pg_catalog.to_regclass('public.latest_messages') IS NOT NULL AND
			pg_catalog.to_regclass('public.consumed_messages') IS NOT NULL AND
			pg_catalog.to_regclass('public.imap_seen_tasks') IS NOT NULL
		)
	) THEN
		RAISE EXCEPTION 'restored schema v7 has an unrecognized compatibility-table set';
	END IF;
	IF restored_schema_version = 8 AND NOT (
		auto_alias_table_count = 2 AND seen_table_count = 2 AND archive_table_count = 2 AND
		pg_catalog.to_regclass('public.alias_creation_schedules') IS NOT NULL AND
		pg_catalog.to_regclass('public.pending_alias_api_keys') IS NOT NULL AND
		pg_catalog.to_regclass('public.latest_messages') IS NOT NULL AND
		pg_catalog.to_regclass('public.consumed_messages') IS NOT NULL AND
		pg_catalog.to_regclass('public.imap_seen_tasks') IS NOT NULL AND
		pg_catalog.to_regclass('public.account_mailbox_settings') IS NOT NULL
	) THEN
		RAISE EXCEPTION 'restored schema v8 is missing required compatibility or mailbox tables';
	END IF;

	SELECT required.table_name || '.' || required.constraint_name
	INTO missing_object
	FROM (VALUES
		('all', 'schema_migrations', 'schema_migrations_pkey', 'p'),
		('all', 'app_metadata', 'app_metadata_pkey', 'p'),
		('all', 'admins', 'admins_pkey', 'p'),
		('all', 'admins', 'admins_username_key', 'u'),
		('all', 'admin_sessions', 'admin_sessions_pkey', 'p'),
		('all', 'accounts', 'accounts_pkey', 'p'),
		('all', 'accounts', 'accounts_email_key', 'u'),
		('all', 'aliases', 'aliases_pkey', 'p'),
		('all', 'aliases', 'aliases_address_key', 'u'),
		('all', 'aliases', 'aliases_api_key_hash_key', 'u'),
		('legacy', 'latest_messages', 'latest_messages_pkey', 'p'),
		('v7', 'archived_messages', 'archived_messages_pkey', 'p'),
		('v7', 'archived_messages', 'archived_messages_account_id_uid_validity_upstream_uid_key', 'u'),
		('v7', 'alias_messages', 'alias_messages_pkey', 'p'),
		('v7', 'alias_messages', 'alias_messages_alias_id_mailbox_uid_key', 'u'),
		('v8', 'account_mailbox_settings', 'account_mailbox_settings_pkey', 'p'),
		('all', 'audit_logs', 'audit_logs_pkey', 'p'),
		('all', 'apple_web_sessions', 'apple_web_sessions_pkey', 'p'),
		('all', 'imap_sync_states', 'imap_sync_states_pkey', 'p'),
		('all', 'data_migrations', 'data_migrations_pkey', 'p'),
		('all', 'admin_sessions', 'admin_sessions_admin_id_fkey', 'f'),
		('all', 'aliases', 'aliases_account_id_fkey', 'f'),
		('legacy', 'latest_messages', 'latest_messages_alias_id_fkey', 'f'),
		('v7', 'archived_messages', 'archived_messages_account_id_fkey', 'f'),
		('v7', 'alias_messages', 'alias_messages_alias_id_fkey', 'f'),
		('v7', 'alias_messages', 'alias_messages_message_id_fkey', 'f'),
		('v8', 'account_mailbox_settings', 'account_mailbox_settings_account_id_fkey', 'f'),
		('all', 'audit_logs', 'audit_logs_admin_id_fkey', 'f'),
		('all', 'apple_web_sessions', 'apple_web_sessions_account_id_fkey', 'f'),
		('all', 'imap_sync_states', 'imap_sync_states_account_id_fkey', 'f')
	) AS required(schema_scope, table_name, constraint_name, constraint_type)
	WHERE (required.schema_scope = 'all' OR
		(required.schema_scope = 'legacy' AND (
			restored_schema_version BETWEEN 4 AND 6 OR
			(restored_schema_version IN (7, 8) AND
			 pg_catalog.to_regclass('public.latest_messages') IS NOT NULL)
		)) OR
		(required.schema_scope = 'v7' AND restored_schema_version IN (7, 8)) OR
		(required.schema_scope = 'v8' AND restored_schema_version = 8))
	AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.pg_constraint AS constraint_state
		JOIN pg_catalog.pg_class AS table_state
			ON table_state.oid = constraint_state.conrelid
		JOIN pg_catalog.pg_namespace AS schema_state
			ON schema_state.oid = table_state.relnamespace
		WHERE schema_state.nspname = 'public'
			AND table_state.relname = required.table_name
			AND constraint_state.conname = required.constraint_name
			AND constraint_state.contype::TEXT = required.constraint_type
			AND constraint_state.convalidated
	)
	LIMIT 1;
	IF missing_object IS NOT NULL THEN
		RAISE EXCEPTION 'restored schema is missing required constraint %', missing_object;
	END IF;

	SELECT required.table_name || '.' || required.index_name
	INTO missing_object
	FROM (VALUES
		('all', 'admin_sessions', 'admin_sessions_expires_at_idx'),
		('all', 'admin_sessions', 'admin_sessions_admin_id_idx'),
		('all', 'accounts', 'accounts_enabled_email_idx'),
		('all', 'aliases', 'aliases_account_id_idx'),
		('all', 'aliases', 'aliases_account_address_idx'),
		('all', 'aliases', 'aliases_enabled_account_address_idx'),
		('v7', 'aliases', 'aliases_oauth_client_id_idx'),
		('v7', 'aliases', 'aliases_imap_password_hash_idx'),
		('v7', 'archived_messages', 'archived_messages_retention_idx'),
		('v7', 'alias_messages', 'alias_messages_alias_uid_idx'),
		('v7', 'alias_messages', 'alias_messages_alias_otp_idx'),
		('v8', 'account_mailbox_settings', 'account_mailbox_settings_type_idx'),
		('v8', 'account_mailbox_settings', 'account_mailbox_settings_custom_suffix_idx'),
		('all', 'audit_logs', 'audit_logs_created_at_idx'),
		('all', 'audit_logs', 'audit_logs_admin_id_idx')
	) AS required(schema_scope, table_name, index_name)
	WHERE (required.schema_scope = 'all' OR
		(required.schema_scope = 'v7' AND restored_schema_version IN (7, 8)) OR
		(required.schema_scope = 'v8' AND restored_schema_version = 8))
	AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.pg_class AS index_state
		JOIN pg_catalog.pg_namespace AS schema_state
			ON schema_state.oid = index_state.relnamespace
		JOIN pg_catalog.pg_index AS index_metadata
			ON index_metadata.indexrelid = index_state.oid
		JOIN pg_catalog.pg_class AS table_state
			ON table_state.oid = index_metadata.indrelid
		WHERE schema_state.nspname = 'public'
			AND table_state.relname = required.table_name
			AND index_state.relname = required.index_name
			AND index_state.relkind IN ('i', 'I')
			AND index_metadata.indisvalid
			AND index_metadata.indisready
	)
	LIMIT 1;
	IF missing_object IS NOT NULL THEN
		RAISE EXCEPTION 'restored schema is missing required index %', missing_object;
	END IF;

	SELECT required.table_name || '.' || required.constraint_name
	INTO missing_object
	FROM (VALUES
		('message', 'latest_messages', 'latest_messages_pkey', ARRAY['alias_id']::TEXT[]),
		('auto', 'alias_creation_schedules', 'alias_creation_schedules_pkey', ARRAY['account_id']::TEXT[]),
		('auto', 'pending_alias_api_keys', 'pending_alias_api_keys_pkey', ARRAY['alias_id']::TEXT[]),
		('seen', 'consumed_messages', 'consumed_messages_pkey', ARRAY['alias_id', 'uid_validity', 'uid']::TEXT[]),
		('seen', 'imap_seen_tasks', 'imap_seen_tasks_pkey', ARRAY['account_id', 'uid_validity', 'uid']::TEXT[]),
		('archive', 'archived_messages', 'archived_messages_pkey', ARRAY['id']::TEXT[]),
		('archive', 'alias_messages', 'alias_messages_pkey', ARRAY['alias_id', 'message_id']::TEXT[]),
		('mailbox', 'account_mailbox_settings', 'account_mailbox_settings_pkey', ARRAY['account_id']::TEXT[])
	) AS required(extension_group, table_name, constraint_name, key_columns)
	WHERE pg_catalog.to_regclass('public.' || required.table_name) IS NOT NULL
		AND (required.extension_group <> 'archive' OR restored_schema_version IN (7, 8))
		AND (required.extension_group <> 'mailbox' OR restored_schema_version = 8)
		AND NOT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_state
			JOIN pg_catalog.pg_class AS table_state
				ON table_state.oid = constraint_state.conrelid
			JOIN pg_catalog.pg_namespace AS schema_state
				ON schema_state.oid = table_state.relnamespace
			WHERE schema_state.nspname = 'public'
				AND table_state.relname = required.table_name
				AND constraint_state.conname = required.constraint_name
				AND constraint_state.contype = 'p'
				AND constraint_state.convalidated
				AND (
					SELECT array_agg(attribute_state.attname::TEXT ORDER BY key_column.ordinality)
					FROM unnest(constraint_state.conkey) WITH ORDINALITY
						AS key_column(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_state
						ON attribute_state.attrelid = constraint_state.conrelid
						AND attribute_state.attnum = key_column.attnum
				) = required.key_columns
		)
	LIMIT 1;
	IF missing_object IS NOT NULL THEN
		RAISE EXCEPTION 'restored schema has invalid primary key columns for %', missing_object;
	END IF;

	SELECT required.table_name || '.' || required.constraint_name
	INTO missing_object
	FROM (VALUES
		('message', 'latest_messages', 'latest_messages_alias_id_fkey', 'aliases', ARRAY['alias_id']::TEXT[], ARRAY['id']::TEXT[]),
		('auto', 'alias_creation_schedules', 'alias_creation_schedules_account_id_fkey', 'accounts', ARRAY['account_id']::TEXT[], ARRAY['id']::TEXT[]),
		('auto', 'pending_alias_api_keys', 'pending_alias_api_keys_alias_id_fkey', 'aliases', ARRAY['alias_id']::TEXT[], ARRAY['id']::TEXT[]),
		('seen', 'consumed_messages', 'consumed_messages_alias_id_fkey', 'aliases', ARRAY['alias_id']::TEXT[], ARRAY['id']::TEXT[]),
		('seen', 'imap_seen_tasks', 'imap_seen_tasks_account_id_fkey', 'accounts', ARRAY['account_id']::TEXT[], ARRAY['id']::TEXT[]),
		('archive', 'archived_messages', 'archived_messages_account_id_fkey', 'accounts', ARRAY['account_id']::TEXT[], ARRAY['id']::TEXT[]),
		('archive', 'alias_messages', 'alias_messages_alias_id_fkey', 'aliases', ARRAY['alias_id']::TEXT[], ARRAY['id']::TEXT[]),
		('archive', 'alias_messages', 'alias_messages_message_id_fkey', 'archived_messages', ARRAY['message_id']::TEXT[], ARRAY['id']::TEXT[]),
		('mailbox', 'account_mailbox_settings', 'account_mailbox_settings_account_id_fkey', 'accounts', ARRAY['account_id']::TEXT[], ARRAY['id']::TEXT[])
	) AS required(extension_group, table_name, constraint_name, referenced_table, local_columns, referenced_columns)
	WHERE pg_catalog.to_regclass('public.' || required.table_name) IS NOT NULL
		AND (required.extension_group <> 'archive' OR restored_schema_version IN (7, 8))
		AND (required.extension_group <> 'mailbox' OR restored_schema_version = 8)
		AND NOT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_constraint AS constraint_state
			JOIN pg_catalog.pg_class AS table_state
				ON table_state.oid = constraint_state.conrelid
			JOIN pg_catalog.pg_namespace AS schema_state
				ON schema_state.oid = table_state.relnamespace
			WHERE schema_state.nspname = 'public'
				AND table_state.relname = required.table_name
				AND constraint_state.conname = required.constraint_name
				AND constraint_state.contype = 'f'
				AND constraint_state.convalidated
				AND constraint_state.confrelid = pg_catalog.to_regclass('public.' || required.referenced_table)
				AND constraint_state.confdeltype = 'c'
				AND (
					SELECT array_agg(attribute_state.attname::TEXT ORDER BY key_column.ordinality)
					FROM unnest(constraint_state.conkey) WITH ORDINALITY
						AS key_column(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_state
						ON attribute_state.attrelid = constraint_state.conrelid
						AND attribute_state.attnum = key_column.attnum
				) = required.local_columns
				AND (
					SELECT array_agg(attribute_state.attname::TEXT ORDER BY key_column.ordinality)
					FROM unnest(constraint_state.confkey) WITH ORDINALITY
						AS key_column(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_state
						ON attribute_state.attrelid = constraint_state.confrelid
						AND attribute_state.attnum = key_column.attnum
				) = required.referenced_columns
		)
	LIMIT 1;
	IF missing_object IS NOT NULL THEN
		RAISE EXCEPTION 'restored schema has invalid foreign key %', missing_object;
	END IF;

	SELECT required.table_name || '.' || required.index_name
	INTO missing_object
	FROM (VALUES
		('auto', 'alias_creation_schedules', 'alias_creation_schedules_due_idx', ARRAY['enabled', 'next_run_at', 'account_id']::TEXT[]),
		('seen', 'imap_seen_tasks', 'imap_seen_tasks_account_created_idx', ARRAY['account_id', 'created_at', 'uid_validity', 'uid']::TEXT[]),
		('archive', 'aliases', 'aliases_imap_password_hash_idx', ARRAY['imap_password_hash']::TEXT[]),
		('archive', 'archived_messages', 'archived_messages_retention_idx', ARRAY['content_state', 'internal_date', 'id']::TEXT[]),
		('archive', 'alias_messages', 'alias_messages_alias_uid_idx', ARRAY['alias_id', 'mailbox_uid']::TEXT[]),
		('mailbox', 'account_mailbox_settings', 'account_mailbox_settings_type_idx', ARRAY['mailbox_type', 'account_id']::TEXT[])
	) AS required(extension_group, table_name, index_name, key_columns)
	WHERE pg_catalog.to_regclass('public.' || required.table_name) IS NOT NULL
		AND (required.extension_group <> 'archive' OR restored_schema_version IN (7, 8))
		AND (required.extension_group <> 'mailbox' OR restored_schema_version = 8)
		AND NOT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_class AS index_state
			JOIN pg_catalog.pg_namespace AS schema_state
				ON schema_state.oid = index_state.relnamespace
			JOIN pg_catalog.pg_index AS index_metadata
				ON index_metadata.indexrelid = index_state.oid
			JOIN pg_catalog.pg_class AS table_state
				ON table_state.oid = index_metadata.indrelid
			JOIN pg_catalog.pg_am AS access_method
				ON access_method.oid = index_state.relam
			WHERE schema_state.nspname = 'public'
				AND table_state.relname = required.table_name
				AND index_state.relname = required.index_name
				AND index_state.relkind IN ('i', 'I')
				AND access_method.amname = 'btree'
				AND index_metadata.indisvalid
				AND index_metadata.indisready
				AND NOT index_metadata.indisunique
				AND index_metadata.indnkeyatts = array_length(required.key_columns, 1)
				AND index_metadata.indnatts = array_length(required.key_columns, 1)
				AND index_metadata.indpred IS NULL
				AND index_metadata.indexprs IS NULL
				AND (
					SELECT array_agg(attribute_state.attname::TEXT ORDER BY key_column.ordinality)
					FROM unnest(index_metadata.indkey::SMALLINT[]) WITH ORDINALITY
						AS key_column(attnum, ordinality)
					JOIN pg_catalog.pg_attribute AS attribute_state
						ON attribute_state.attrelid = index_metadata.indrelid
						AND attribute_state.attnum = key_column.attnum
				) = required.key_columns
		)
	LIMIT 1;
	IF missing_object IS NOT NULL THEN
		RAISE EXCEPTION 'restored schema has invalid index %', missing_object;
	END IF;

	IF restored_schema_version = 8 AND NOT EXISTS (
		SELECT 1
		FROM pg_catalog.pg_class AS index_state
		JOIN pg_catalog.pg_namespace AS schema_state
			ON schema_state.oid = index_state.relnamespace
		JOIN pg_catalog.pg_index AS index_metadata
			ON index_metadata.indexrelid = index_state.oid
		JOIN pg_catalog.pg_class AS table_state
			ON table_state.oid = index_metadata.indrelid
		JOIN pg_catalog.pg_am AS access_method
			ON access_method.oid = index_state.relam
		WHERE schema_state.nspname = 'public'
			AND table_state.relname = 'account_mailbox_settings'
			AND index_state.relname = 'account_mailbox_settings_custom_suffix_idx'
			AND index_state.relkind IN ('i', 'I')
			AND access_method.amname = 'btree'
			AND index_metadata.indisvalid
			AND index_metadata.indisready
			AND index_metadata.indisunique
			AND index_metadata.indnkeyatts = 1
			AND index_metadata.indnatts = 1
			AND index_metadata.indpred IS NOT NULL
			AND index_metadata.indexprs IS NOT NULL
			AND (
				SELECT array_agg(key_column.attnum ORDER BY key_column.ordinality)
				FROM unnest(index_metadata.indkey::SMALLINT[]) WITH ORDINALITY
					AS key_column(attnum, ordinality)
			) = ARRAY[0]::SMALLINT[]
			AND regexp_replace(
				lower(pg_catalog.pg_get_expr(index_metadata.indexprs, index_metadata.indrelid, true)),
				'[[:space:]()]', '', 'g'
			) = 'loweremail_suffix'
			AND regexp_replace(
				lower(pg_catalog.pg_get_expr(index_metadata.indpred, index_metadata.indrelid, true)),
				'[[:space:]()]', '', 'g'
			) = 'mailbox_type=''custom''::text'
	) THEN
		RAISE EXCEPTION 'restored schema has invalid index account_mailbox_settings.account_mailbox_settings_custom_suffix_idx';
	END IF;
END
$icloud_api_restore_validation$;
SQL
			printf '%s\n' 'REVOKE CREATE ON SCHEMA public FROM PUBLIC;'
		} > "$restore_batch" || die "组装 PostgreSQL 恢复事务失败"

		if psql -h /var/run/postgresql -U "$LOADED_USER" -d "$LOADED_DATABASE" \
			--no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction \
			--file="$restore_batch"; then
			exit 0
		else
			exit "$?"
		fi
		;;
	psql)
		shift
		load_credentials "$credentials_file"
		PGPASSWORD="$LOADED_PASSWORD"
		export PGPASSWORD
		exec psql -h /var/run/postgresql -U "$LOADED_USER" -d "$LOADED_DATABASE" "$@"
		;;
esac

mkdir -p "$installation_state_directory" || die "创建安装状态目录失败"
chmod 755 "$installation_state_directory" || die "设置安装状态目录权限失败"
exec 9>"${installation_state_directory}/postgres-lifecycle.lock"
flock -n 9 || die "另一个 PostgreSQL 容器正在使用当前数据卷"

if [ "${1:-}" = "prepare-restore" ]; then
	[ "$postgres_data" = "/var/lib/postgresql/data" ] || die "prepare-restore 仅允许清理 Compose 固定的 PGDATA 路径"
	[ -d "$postgres_data" ] && [ ! -L "$postgres_data" ] || die "PGDATA 不是预期的普通目录"
	[ "$(readlink -f "$postgres_data")" = "/var/lib/postgresql/data" ] || die "PGDATA 解析后的路径不符合预期"
	postgres_data_has_entries=false
	if find "$postgres_data" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
		postgres_data_has_entries=true
	fi
	if [ "$postgres_data_has_entries" = true ]; then
		for completed_marker in \
			"$state_file" \
			"$cluster_initialized_marker" \
			"$config_cluster_initialized_marker" \
			"$initialized_marker"; do
			if [ -e "$completed_marker" ] || [ -L "$completed_marker" ]; then
				die "持久化状态表明数据库已完成初始化或状态异常；拒绝清理 postgres_data"
			fi
		done
		[ -f "$credentials_file" ] && [ ! -L "$credentials_file" ] || \
			die "缺少可验证的首次初始化凭据状态；拒绝清理 postgres_data"
		[ "$(stat -c '%h' "$credentials_file")" = "1" ] || \
			die "首次初始化凭据状态不能是硬链接"
		load_credentials "$credentials_file"
		validate_bound_marker "$config_cluster_bootstrap_marker" "$LOADED_INSTALLATION_ID" \
			"数据库配置首次初始化标记"
		validate_bound_marker "$cluster_bootstrap_marker" "$LOADED_INSTALLATION_ID" \
			"安装状态首次初始化标记"
		validate_bound_marker "$bootstrap_marker" "$LOADED_INSTALLATION_ID" \
			"主密钥首次初始化标记"
		pgdata_root_identity="$(stat -Lc '%d:%i' "$postgres_data")" || \
			die "读取 PGDATA 根目录标识失败"
		pgdata_binding="${LOADED_INSTALLATION_ID}:${pgdata_root_identity}"
		validate_bound_marker "$config_pgdata_bootstrap_binding" "$pgdata_binding" \
			"数据库配置 PGDATA 绑定标记"
		validate_bound_marker "$cluster_pgdata_bootstrap_binding" "$pgdata_binding" \
			"安装状态 PGDATA 绑定标记"
		find "$postgres_data" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} \;
		if find "$postgres_data" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
			die "清理未完成的 PostgreSQL 初始化目录失败"
		fi
		printf '%s\n' "PostgreSQL 未完成的首次初始化数据已清理。" >&2
	fi
	rm -f "$state_file"
	rm -f "$credentials_file"
	rm -f "$config_cluster_bootstrap_marker" "$config_cluster_initialized_marker"
	rm -f "$config_pgdata_bootstrap_binding"
	rm -f "$bootstrap_marker" "$initialized_marker"
	rm -f "$cluster_bootstrap_marker" "$cluster_initialized_marker"
	rm -f "$cluster_pgdata_bootstrap_binding"
	sync || die "持久化 PostgreSQL 恢复状态清理失败"
	printf '%s\n' "PostgreSQL 恢复状态已重置；现在可启动 postgres 并导入逻辑备份。" >&2
	exit 0
fi

config_exists=false
state_exists=false
postgres_initialized=false
postgres_data_has_entries=false
[ -f "$credentials_file" ] && config_exists=true
[ -f "$state_file" ] && state_exists=true
[ -s "${postgres_data}/PG_VERSION" ] && postgres_initialized=true
if [ -d "$postgres_data" ] && find "$postgres_data" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
	postgres_data_has_entries=true
fi

if [ "$state_exists" = true ] && [ "$postgres_initialized" = false ]; then
	die "数据库状态文件存在但 PGDATA 未初始化；请成对恢复 postgres_data 与 postgres_config 卷"
fi
if [ "$state_exists" = false ] && [ "$postgres_data_has_entries" = true ]; then
	if { [ -e "$cluster_bootstrap_marker" ] || [ -e "$config_cluster_bootstrap_marker" ]; } && \
		[ ! -e "$cluster_initialized_marker" ] && [ ! -e "$config_cluster_initialized_marker" ]; then
		die "检测到未完成的首次 PostgreSQL 初始化；请停止服务后执行 prepare-restore，再重新启动"
	fi
	die "PGDATA 包含数据但缺少已完成初始化的凭据状态；请恢复匹配的 postgres_data 卷"
fi

if [ "$config_exists" = true ]; then
	load_credentials "$credentials_file"
	config_user="$LOADED_USER"
	config_database="$LOADED_DATABASE"
	config_password="$LOADED_PASSWORD"
	config_installation_id="$LOADED_INSTALLATION_ID"
fi

if [ "$config_exists" = true ] && [ "$state_exists" = true ] && \
	[ "$postgres_initialized" = true ] && [ -f "$state_file" ] && [ ! -L "$state_file" ]; then
	legacy_state_installation_id="$(sed -n '4p' "$state_file")"
	case "$legacy_state_installation_id" in
		"${config_installation_id}:"*)
			pgdata_root_identity="$(stat -Lc '%d:%i' "$postgres_data")" || \
				die "读取 PGDATA 根目录标识失败"
			legacy_pgdata_binding="${config_installation_id}:${pgdata_root_identity}"
			if [ "$legacy_state_installation_id" = "$legacy_pgdata_binding" ]; then
				repair_legacy_pgdata_binding_state \
					"$config_user" \
					"$config_database" \
					"$config_password" \
					"$config_installation_id" \
					"$legacy_pgdata_binding"
			fi
			;;
	esac
fi

if [ "$state_exists" = true ]; then
	load_credentials "$state_file"
	state_user="$LOADED_USER"
	state_database="$LOADED_DATABASE"
	state_password="$LOADED_PASSWORD"
	state_installation_id="$LOADED_INSTALLATION_ID"
fi

if [ "$config_exists" = true ] && [ "$state_exists" = true ]; then
	[ "$config_user" = "$state_user" ] && \
		[ "$config_database" = "$state_database" ] && \
		[ "$config_password" = "$state_password" ] && \
		[ "$config_installation_id" = "$state_installation_id" ] || \
		die "postgres_data 与 postgres_config 凭据不一致；请成对恢复两个卷"
	database_user="$config_user"
	database_name="$config_database"
	database_password="$config_password"
	installation_id="$config_installation_id"
elif [ "$state_exists" = true ]; then
	database_user="$state_user"
	database_name="$state_database"
	database_password="$state_password"
	installation_id="$state_installation_id"
	write_shared_credentials "$database_user" "$database_name" "$database_password" "$installation_id"
elif [ "$config_exists" = true ]; then
	[ "$postgres_initialized" = false ] || die "已初始化的 PGDATA 缺少凭据状态；请恢复匹配的 postgres_config 卷"
	if [ ! -e "$config_cluster_bootstrap_marker" ] && [ ! -e "$cluster_bootstrap_marker" ]; then
		die "postgres_config 已存在但数据库与安装状态均为空；请恢复原卷，或先执行 prepare-restore 再导入逻辑备份"
	fi
	database_user="$config_user"
	database_name="$config_database"
	database_password="$config_password"
	installation_id="$config_installation_id"
else
	[ "$postgres_initialized" = false ] || die "已初始化的 PGDATA 缺少持久化凭据；请恢复 postgres_config 卷"
	database_user="icloud_$(random_hex 8)"
	database_name="icloud_$(random_hex 8)"
	database_password="$(random_hex 32)"
	installation_id="$(random_hex 16)"
	write_shared_credentials "$database_user" "$database_name" "$database_password" "$installation_id"
	write_cluster_marker "$config_cluster_bootstrap_marker" "$installation_id"
fi

for marker in "$bootstrap_marker" "$initialized_marker"; do
	if [ -e "$marker" ]; then
		[ "$(wc -l < "$marker" | tr -d ' ')" = "1" ] || die "主密钥状态文件格式错误"
		[ "$(sed -n '1p' "$marker")" = "$installation_id" ] || die "主密钥状态与当前数据库不匹配"
	fi
done

for marker in "$cluster_bootstrap_marker" "$cluster_initialized_marker"; do
	if [ -e "$marker" ]; then
		[ "$(wc -l < "$marker" | tr -d ' ')" = "1" ] || die "数据库集群状态文件格式错误"
		[ "$(sed -n '1p' "$marker")" = "$installation_id" ] || die "数据库集群状态与凭据不匹配"
	fi
done

for marker in "$config_cluster_bootstrap_marker" "$config_cluster_initialized_marker"; do
	if [ -e "$marker" ]; then
		[ "$(wc -l < "$marker" | tr -d ' ')" = "1" ] || die "数据库配置阶段文件格式错误"
		[ "$(sed -n '1p' "$marker")" = "$installation_id" ] || die "数据库配置阶段与凭据不匹配"
	fi
done

if [ -e "$cluster_initialized_marker" ] && [ "$postgres_initialized" = false ]; then
	die "installation_state 表明数据库已初始化，但 postgres_data 为空；请恢复原 postgres_data 卷"
fi
if [ -e "$initialized_marker" ] && [ "$postgres_initialized" = false ]; then
	die "应用主密钥已绑定到原数据库，但 postgres_data 为空；请恢复原 postgres_data 卷"
fi
if [ -e "$config_cluster_initialized_marker" ] && [ "$postgres_initialized" = false ]; then
	die "postgres_config 表明数据库曾完成初始化，但 postgres_data 为空；请恢复原卷，或执行 prepare-restore 后导入逻辑备份"
fi

if [ "$state_exists" = true ]; then
	write_cluster_marker "$config_cluster_initialized_marker" "$installation_id"
	rm -f "$config_cluster_bootstrap_marker"
elif [ ! -e "$config_cluster_bootstrap_marker" ]; then
	write_cluster_marker "$config_cluster_bootstrap_marker" "$installation_id"
fi

if [ -e "$cluster_bootstrap_marker" ] && [ "$postgres_initialized" = true ] && [ "$state_exists" = true ]; then
	write_cluster_marker "$cluster_initialized_marker" "$installation_id"
	rm -f "$cluster_bootstrap_marker"
fi
if [ ! -e "$cluster_bootstrap_marker" ] && [ ! -e "$cluster_initialized_marker" ]; then
	if [ "$state_exists" = true ] || [ "$postgres_initialized" = true ]; then
		write_cluster_marker "$cluster_initialized_marker" "$installation_id"
	else
		write_cluster_marker "$cluster_bootstrap_marker" "$installation_id"
	fi
fi

if [ ! -e "$bootstrap_marker" ] && [ ! -e "$initialized_marker" ]; then
	# PostgreSQL lifecycle state cannot prove that the application has parsed
	# and verified its master key. Only the application preflight promotes this
	# marker to key-initialized after fingerprint verification succeeds.
	write_key_marker "$bootstrap_marker" "$installation_id"
fi

if [ "$postgres_initialized" = false ] && [ "$state_exists" = false ]; then
	mkdir -p "$postgres_data" || die "创建 PGDATA 目录失败"
	[ -d "$postgres_data" ] && [ ! -L "$postgres_data" ] || die "PGDATA 不是普通目录"
	pgdata_root_identity="$(stat -Lc '%d:%i' "$postgres_data")" || die "读取 PGDATA 根目录标识失败"
	pgdata_binding="${installation_id}:${pgdata_root_identity}"
	for binding_marker in "$config_pgdata_bootstrap_binding" "$cluster_pgdata_bootstrap_binding"; do
		if [ -e "$binding_marker" ] || [ -L "$binding_marker" ]; then
			validate_bound_marker "$binding_marker" "$pgdata_binding" "PGDATA 首次初始化绑定标记"
		else
			write_cluster_marker "$binding_marker" "$pgdata_binding"
		fi
	done
fi

# Credentials and every pre-init marker must reach their backing volumes before
# initdb can leave a partially populated PGDATA directory.
sync || die "持久化 PostgreSQL 首次初始化状态失败"

[ "${#installation_id}" -eq 32 ] || die "内部安装标识格式错误"
case "$installation_id" in
	*[!0-9a-f]*) die "内部安装标识格式错误" ;;
esac

POSTGRES_DB="$database_name"
POSTGRES_APP_USER="$database_user"
POSTGRES_APP_PASSWORD="$database_password"
ICLOUD_API_INSTALLATION_ID="$installation_id"
if [ "$postgres_initialized" = false ]; then
	POSTGRES_USER="bootstrap_$(random_hex 8)"
	POSTGRES_PASSWORD="$(random_hex 32)"
else
	# The official entrypoint ignores initialization variables for an existing
	# cluster. Keep them valid without retaining bootstrap credentials.
	POSTGRES_USER="$database_user"
	POSTGRES_PASSWORD="$database_password"
fi
export POSTGRES_USER POSTGRES_DB POSTGRES_PASSWORD POSTGRES_APP_USER POSTGRES_APP_PASSWORD ICLOUD_API_INSTALLATION_ID

exec "${POSTGRES_OFFICIAL_ENTRYPOINT:-/usr/local/bin/docker-entrypoint.sh}" "$@"
