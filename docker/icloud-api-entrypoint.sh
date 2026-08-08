#!/bin/sh
set -eu
umask 077

die() {
	printf '%s\n' "icloud-api 启动失败：$*" >&2
	exit 1
}

has_text() {
	case "${1:-}" in
		*[![:space:]]*) return 0 ;;
		*) return 1 ;;
	esac
}

byte_length() {
	LC_ALL=C printf '%s' "$1" | wc -c | tr -d ' '
}

validate_admin_username() {
	value="$1"
	has_text "$value" || die "ICLOUD_API_ADMIN_USER 不能为空"
	[ "$(byte_length "$value")" -le 128 ] || die "ICLOUD_API_ADMIN_USER 不能超过 128 字节"
}

validate_admin_password() {
	value="$1"
	length="$(byte_length "$value")"
	[ "$length" -ge 12 ] || die "ICLOUD_API_ADMIN_PASSWORD 至少需要 12 字节"
	[ "$length" -le 72 ] || die "ICLOUD_API_ADMIN_PASSWORD 不能超过 72 字节"
}

validate_oauth_token() {
	value="$1"
	length="$(byte_length "$value")"
	[ "$length" -ge 32 ] && [ "$length" -le 4096 ] || \
		die "ICLOUD_API_OAUTH_TOKEN 必须为 32 到 4096 个不含空白的字符"
	case "$value" in
		*[[:space:]]*) die "ICLOUD_API_OAUTH_TOKEN 必须为 32 到 4096 个不含空白的字符" ;;
	esac
}

random_hex() {
	byte_count="$1"
	value="$(od -An -N "$byte_count" -tx1 /dev/urandom | tr -d ' \n')"
	[ "${#value}" -eq "$((byte_count * 2))" ] || die "生成随机凭据失败"
	printf '%s' "$value"
}

write_secret() {
	path="$1"
	value="$2"
	directory="${path%/*}"
	[ "$directory" != "$path" ] || directory="."
	mkdir -p "$directory" || die "创建凭据目录失败：$directory"
	chmod 700 "$directory" || die "设置凭据目录权限失败：$directory"
	temporary="$(mktemp "${path}.tmp.XXXXXX")" || die "创建凭据临时文件失败：$path"
	umask 077
	printf '%s\n' "$value" > "$temporary" || { rm -f "$temporary"; die "写入凭据临时文件失败：$path"; }
	chmod 600 "$temporary" || { rm -f "$temporary"; die "设置凭据文件权限失败：$path"; }
	mv -f "$temporary" "$path" || { rm -f "$temporary"; die "发布凭据文件失败：$path"; }
}

install_secret_if_absent() {
	path="$1"
	value="$2"
	directory="${path%/*}"
	[ "$directory" != "$path" ] || directory="."
	mkdir -p "$directory" || die "创建凭据目录失败：$directory"
	chmod 700 "$directory" || die "设置凭据目录权限失败：$directory"
	temporary="$(mktemp "${path}.tmp.XXXXXX")" || die "创建凭据临时文件失败：$path"
	umask 077
	printf '%s\n' "$value" > "$temporary" || { rm -f "$temporary"; die "写入凭据临时文件失败：$path"; }
	chmod 600 "$temporary" || { rm -f "$temporary"; die "设置凭据文件权限失败：$path"; }
	if ln "$temporary" "$path" 2>/dev/null; then
		rm -f "$temporary" || die "清理凭据临时文件失败：$temporary"
		return 0
	fi
	if [ -e "$path" ]; then
		rm -f "$temporary" || die "清理凭据临时文件失败：$temporary"
		return 1
	fi
	rm -f "$temporary" || true
	die "原子发布凭据文件失败：$path"
}

install_file_if_absent() {
	source="$1"
	path="$2"
	directory="${path%/*}"
	[ "$directory" != "$path" ] || directory="."
	mkdir -p "$directory" || die "创建凭据目录失败：$directory"
	chmod 700 "$directory" || die "设置凭据目录权限失败：$directory"
	temporary="$(mktemp "${path}.tmp.XXXXXX")" || die "创建凭据临时文件失败：$path"
	umask 077
	cp "$source" "$temporary" || { rm -f "$temporary"; die "复制旧主密钥失败"; }
	chmod 600 "$temporary" || { rm -f "$temporary"; die "设置主密钥文件权限失败"; }
	if ln "$temporary" "$path" 2>/dev/null; then
		rm -f "$temporary" || die "清理主密钥临时文件失败"
		return 0
	fi
	if [ -e "$path" ]; then
		rm -f "$temporary" || die "清理主密钥临时文件失败"
		return 1
	fi
	rm -f "$temporary" || true
	die "原子发布主密钥失败：$path"
}

read_hex_secret() {
	path="$1"
	expected_length="$2"
	[ -f "$path" ] || die "凭据文件不存在：$path"
	[ "$(wc -l < "$path" | tr -d ' ')" = "1" ] || die "凭据文件格式错误：$path"
	[ "$(wc -c < "$path" | tr -d ' ')" -eq "$((expected_length + 1))" ] || \
		die "凭据文件格式错误：$path"
	value="$(sed -n '1p' "$path")"
	[ "${#value}" -eq "$expected_length" ] || die "凭据文件长度错误：$path"
	case "$value" in
		*[!0-9a-f]*) die "凭据文件包含非法字符：$path" ;;
	esac
	printf '%s' "$value"
}

load_database_url() {
	credential_file="${ICLOUD_API_DATABASE_CONFIG_DIR:-/run/icloud-api-database}/credentials"
	[ -f "$credential_file" ] || die "数据库凭据尚未生成：$credential_file"
	[ "$(wc -l < "$credential_file" | tr -d ' ')" = "4" ] || die "数据库凭据文件格式错误"

	database_user="$(sed -n '1p' "$credential_file")"
	database_name="$(sed -n '2p' "$credential_file")"
	database_password="$(sed -n '3p' "$credential_file")"
	ICLOUD_API_INSTALLATION_ID="$(sed -n '4p' "$credential_file")"
	case "$database_user" in
		[a-z][a-z0-9_]*) ;;
		*) die "数据库用户名格式错误" ;;
	esac
	case "$database_user" in
		*[!a-z0-9_]*) die "数据库用户名格式错误" ;;
	esac
	case "$database_name" in
		[a-z][a-z0-9_]*) ;;
		*) die "数据库名格式错误" ;;
	esac
	case "$database_name" in
		*[!a-z0-9_]*) die "数据库名格式错误" ;;
	esac
	[ "${#database_password}" -eq 64 ] || die "数据库密码格式错误"
	case "$database_password" in
		*[!0-9a-f]*) die "数据库密码格式错误" ;;
	esac
	[ "${#ICLOUD_API_INSTALLATION_ID}" -eq 32 ] || die "安装标识格式错误"
	case "$ICLOUD_API_INSTALLATION_ID" in
		*[!0-9a-f]*) die "安装标识格式错误" ;;
	esac

	ICLOUD_API_DATABASE_URL="postgres://${database_user}:${database_password}@/${database_name}?host=/var/run/postgresql&sslmode=disable"
	export ICLOUD_API_DATABASE_URL ICLOUD_API_INSTALLATION_ID
}

acquire_admin_reset_locks() {
	app_state_directory="${ICLOUD_API_INSTALLATION_STATE_DIR:-/run/icloud-api-installation}/app-state"
	maintenance_lock_file="${app_state_directory}/maintenance.lock"
	maintenance_window_lock_file="${app_state_directory}/maintenance-window.lock"
	keys_directory="${ICLOUD_API_KEYS_DIR:-/app/keys}"
	[ -d "$app_state_directory" ] && [ ! -L "$app_state_directory" ] || \
		die "安装状态目录不存在或不是普通目录：$app_state_directory"
	[ -d "$keys_directory" ] && [ ! -L "$keys_directory" ] || \
		die "密钥目录不存在或不是普通目录：$keys_directory"
	if [ -e "$maintenance_lock_file" ] || [ -L "$maintenance_lock_file" ]; then
		[ -f "$maintenance_lock_file" ] && [ ! -L "$maintenance_lock_file" ] || \
			die "维护锁不是普通文件：$maintenance_lock_file"
	fi

	case "${ICLOUD_API_ADMIN_RESET_LOCK_CONTEXT:-}" in
		"")
			exec 8>"$maintenance_lock_file" || die "打开管理员重置维护锁失败"
			flock -n 8 || die "另一个管理员重置、备份或恢复任务正在运行"
			exec 9<"$keys_directory" || die "打开管理员重置密钥锁失败"
			flock -n 9 || die "另一个密钥备份或恢复任务正在运行"
			;;
		keys-maintenance-restore-v1)
			[ -e /proc/self/fd/9 ] || die "密钥恢复没有继承受保护的密钥锁"
			keys_fd_identity="$(stat -Lc '%d:%i' /proc/self/fd/9 2>/dev/null)" || \
				die "读取继承的密钥锁失败"
			keys_directory_identity="$(stat -Lc '%d:%i' "$keys_directory" 2>/dev/null)" || \
				die "读取密钥目录标识失败"
			[ "$keys_fd_identity" = "$keys_directory_identity" ] || \
				die "继承的密钥锁与当前 keys 卷不匹配"
			flock -n 9 || die "继承的密钥锁未保持独占状态"
			[ -f "$maintenance_window_lock_file" ] && [ ! -L "$maintenance_window_lock_file" ] || \
				die "维护窗口上下文锁不存在或不是普通文件"
			exec 7>"$maintenance_window_lock_file" || die "打开维护窗口上下文锁失败"
			if flock -n 7; then
				flock -u 7 || true
				die "密钥恢复的管理员重置缺少宿主维护窗口上下文"
			fi
			exec 8>"$maintenance_lock_file" || die "打开管理员重置维护锁失败"
			if flock -n 8; then
				flock -u 8 || true
				die "密钥恢复的管理员重置必须位于宿主维护窗口内"
			fi
			;;
		*)
			die "管理员重置锁上下文无效"
			;;
	esac
	unset ICLOUD_API_ADMIN_RESET_LOCK_CONTEXT
}

bootstrap_master_key() {
	ICLOUD_API_FRESH_BOOTSTRAP=false
	ICLOUD_API_KEY_BOOTSTRAP_PENDING=false
	master_key_file="${ICLOUD_API_MASTER_KEY_FILE:-/app/keys/master.key}"
	if ! has_text "${ICLOUD_API_MASTER_KEY:-}" && \
		[ -e "$master_key_file" ] && [ ! -s "$master_key_file" ]; then
		die "主密钥文件为空：$master_key_file"
	fi

	legacy_database="${ICLOUD_API_LEGACY_SQLITE:-}"
	legacy_key="${legacy_database}.key"

	# Native image users may provide their own database URL without the Compose
	# installation state volume. Preserve the normal application key fallback.
	if [ -z "${ICLOUD_API_INSTALLATION_ID:-}" ]; then
		has_text "${ICLOUD_API_MASTER_KEY:-}" && return 0
		[ ! -s "$master_key_file" ] || return 0
		if [ -n "$legacy_database" ] && [ -e "$legacy_database" ] && [ -s "$legacy_key" ]; then
			if install_file_if_absent "$legacy_key" "$master_key_file"; then
				printf '%s\n' "icloud-api：已从旧 SQLite 数据卷迁移主密钥。" >&2
			fi
			return 0
		fi
		if [ -n "$legacy_database" ] && [ -e "$legacy_database" ]; then
			die "检测到旧 SQLite 数据库，但未找到 ${legacy_key}；请继续提供原 ICLOUD_API_MASTER_KEY"
		fi
		return 0
	fi

	key_state_directory="${ICLOUD_API_INSTALLATION_STATE_DIR:-/run/icloud-api-installation}/app-state"
	bootstrap_marker="${key_state_directory}/allow-key-bootstrap"
	initialized_marker="${key_state_directory}/key-initialized"
	for marker in "$bootstrap_marker" "$initialized_marker"; do
		if [ -e "$marker" ]; then
			[ "$(wc -l < "$marker" | tr -d ' ')" = "1" ] || die "主密钥状态文件格式错误：$marker"
			[ "$(sed -n '1p' "$marker")" = "$ICLOUD_API_INSTALLATION_ID" ] || die "主密钥状态与当前数据库不匹配"
		fi
	done

	if [ -e "$initialized_marker" ]; then
		if ! has_text "${ICLOUD_API_MASTER_KEY:-}" && [ ! -s "$master_key_file" ]; then
			die "数据库已绑定主密钥，但 keys 卷中的 ${master_key_file} 丢失；请恢复 keys 卷或提供原 ICLOUD_API_MASTER_KEY"
		fi
		rm -f "$bootstrap_marker"
		return 0
	fi

	[ -e "$bootstrap_marker" ] || die "数据库缺少主密钥引导状态；为防止生成错误密钥，启动已停止"
	ICLOUD_API_FRESH_BOOTSTRAP=true
	if has_text "${ICLOUD_API_MASTER_KEY:-}" || [ -s "$master_key_file" ]; then
		:
	elif [ -n "$legacy_database" ] && [ -e "$legacy_database" ] && [ -s "$legacy_key" ]; then
		if install_file_if_absent "$legacy_key" "$master_key_file"; then
			printf '%s\n' "icloud-api：已从旧 SQLite 数据卷迁移主密钥。" >&2
		fi
	else
		if [ -n "$legacy_database" ] && [ -e "$legacy_database" ]; then
			die "检测到旧 SQLite 数据库，但未找到 ${legacy_key}；请继续提供原 ICLOUD_API_MASTER_KEY"
		else
			generated_master_key="$(random_hex 32)"
			install_secret_if_absent "$master_key_file" "$generated_master_key" || true
		fi
	fi

	ICLOUD_API_KEY_BOOTSTRAP_PENDING=true
}

commit_key_bootstrap() {
	[ "${ICLOUD_API_KEY_BOOTSTRAP_PENDING:-false}" = "true" ] || return 0
	write_secret "$initialized_marker" "$ICLOUD_API_INSTALLATION_ID"
	sync || die "持久化主密钥初始化完成标记失败"
	rm -f "$bootstrap_marker"
	sync || die "持久化主密钥初始化状态清理失败"
}

commit_admin_reset_secret() {
	case "${ICLOUD_API_ADMIN_RESET_SECRET_COMMIT:-none}" in
		none)
			return 0
			;;
		environment)
			write_secret "$ICLOUD_API_ADMIN_RESET_SOURCE_MARKER" "environment"
			rm -f "$ICLOUD_API_ADMIN_RESET_PASSWORD_FILE" || die "清理旧管理员密码文件失败"
			rm -f "$ICLOUD_API_ADMIN_RESET_PENDING_FILE" || die "清理管理员重置临时密码失败"
			;;
		file)
			[ -f "$ICLOUD_API_ADMIN_RESET_PENDING_FILE" ] || die "管理员重置临时密码丢失"
			chmod 600 "$ICLOUD_API_ADMIN_RESET_PENDING_FILE" || die "设置管理员重置临时密码权限失败"
			rm -f "$ICLOUD_API_ADMIN_RESET_SOURCE_MARKER" || die "清理旧管理员密码来源失败"
			mv -f "$ICLOUD_API_ADMIN_RESET_PENDING_FILE" "$ICLOUD_API_ADMIN_RESET_PASSWORD_FILE" || \
				die "提交管理员重置密码失败"
			printf '%s\n' "icloud-api：已生成管理员密码并保存到 ${ICLOUD_API_ADMIN_RESET_PASSWORD_FILE}（权限 0600，不写入日志）。" >&2
			printf '%s\n' "icloud-api：查看命令：docker compose exec -T icloud-api cat ${ICLOUD_API_ADMIN_RESET_PASSWORD_FILE}" >&2
			;;
		*)
			die "管理员密码提交状态错误"
			;;
	esac
	sync || die "持久化管理员重置凭据失败"
}

commit_admin_bootstrap_secret() {
	status="$1"
	case "$status" in
		admin-required|admin-existing)
			;;
		*)
			die "应用启动预检返回了未知状态"
			;;
	esac
	[ "${ICLOUD_API_ADMIN_PASSWORD_PENDING:-false}" = "true" ] || return 0

	if [ "$status" = "admin-required" ]; then
		if install_secret_if_absent "$admin_password_file" "$ICLOUD_API_ADMIN_PASSWORD"; then
			printf '%s\n' "icloud-api：已生成管理员密码并保存到 ${admin_password_file}（权限 0600，不写入日志）。" >&2
			printf '%s\n' "icloud-api：查看命令：docker compose exec -T icloud-api cat ${admin_password_file}" >&2
		else
			ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_password_file" 48)"
			export ICLOUD_API_ADMIN_PASSWORD
		fi
		rm -f "$admin_source_marker" || die "清理旧管理员密码来源失败"
	else
		write_secret "$admin_source_marker" "legacy"
		rm -f "$admin_password_file" || die "清理未使用的管理员密码文件失败"
		unset ICLOUD_API_ADMIN_PASSWORD
	fi
	sync || die "持久化管理员初始化凭据失败"
}

load_or_generate_application_secrets() {
	keys_directory="${ICLOUD_API_KEYS_DIR:-/app/keys}"
	admin_password_file="${keys_directory}/admin-password"
	admin_pending_password_file="${keys_directory}/admin-password.pending"
	admin_source_marker="${keys_directory}/admin-password.source"
	oauth_token_file="${keys_directory}/oauth-token"
	oauth_source_marker="${keys_directory}/oauth-token.source"
	ICLOUD_API_ADMIN_PASSWORD_PENDING=false
	admin_reset=false
	if [ "${1:-}" = "admin" ] && [ "${2:-}" = "reset" ]; then
		admin_reset=true
	fi
	validate_admin_username "${ICLOUD_API_ADMIN_USER:-admin}"
	if has_text "${ICLOUD_API_ADMIN_PASSWORD:-}"; then
		validate_admin_password "$ICLOUD_API_ADMIN_PASSWORD"
	fi
	if [ "$admin_reset" = false ] && has_text "${ICLOUD_API_OAUTH_TOKEN:-}"; then
		validate_oauth_token "$ICLOUD_API_OAUTH_TOKEN"
	fi

	admin_source=""
	if [ -e "$admin_source_marker" ]; then
		[ "$(wc -l < "$admin_source_marker" | tr -d ' ')" = "1" ] || die "管理员密码来源状态文件格式错误"
		admin_source="$(sed -n '1p' "$admin_source_marker")"
		[ "$(wc -c < "$admin_source_marker" | tr -d ' ')" -eq \
			"$(( $(byte_length "$admin_source") + 1 ))" ] || die "管理员密码来源状态文件格式错误"
		[ "$admin_source" = "legacy" ] || [ "$admin_source" = "environment" ] || die "管理员密码来源状态文件格式错误"
	fi
	oauth_source=""
	if [ -e "$oauth_source_marker" ]; then
		[ "$(wc -l < "$oauth_source_marker" | tr -d ' ')" = "1" ] || die "OAuth Token 来源状态文件格式错误"
		oauth_source="$(sed -n '1p' "$oauth_source_marker")"
		[ "$(wc -c < "$oauth_source_marker" | tr -d ' ')" -eq \
			"$(( $(byte_length "$oauth_source") + 1 ))" ] || die "OAuth Token 来源状态文件格式错误"
		[ "$oauth_source" = "environment" ] || die "OAuth Token 来源状态文件格式错误"
	fi

	if [ "$admin_reset" = true ]; then
		if [ -n "$admin_source" ] && [ -e "$admin_password_file" ]; then
			if ! has_text "${ICLOUD_API_ADMIN_PASSWORD:-}" || [ "$admin_source" != "environment" ]; then
				die "管理员密码文件与来源状态冲突；请恢复完整且匹配的 keys 卷"
			fi
		fi
		ICLOUD_API_ADMIN_RESET_PASSWORD_FILE="$admin_password_file"
		ICLOUD_API_ADMIN_RESET_PENDING_FILE="$admin_pending_password_file"
		ICLOUD_API_ADMIN_RESET_SOURCE_MARKER="$admin_source_marker"
		ICLOUD_API_ADMIN_RESET_SECRET_COMMIT=none
		if has_text "${ICLOUD_API_ADMIN_PASSWORD:-}"; then
			ICLOUD_API_ADMIN_RESET_SECRET_COMMIT=environment
		elif [ -e "$admin_pending_password_file" ]; then
			ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_pending_password_file" 48)"
			ICLOUD_API_ADMIN_RESET_SECRET_COMMIT=file
		elif [ -z "$admin_source" ] && [ -e "$admin_password_file" ]; then
			ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_password_file" 48)"
		else
			candidate="$(random_hex 24)"
			if install_secret_if_absent "$admin_pending_password_file" "$candidate"; then
				ICLOUD_API_ADMIN_PASSWORD="$candidate"
			else
				ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_pending_password_file" 48)"
			fi
			ICLOUD_API_ADMIN_RESET_SECRET_COMMIT=file
		fi
		export ICLOUD_API_ADMIN_PASSWORD
		return 0
	fi
	[ ! -e "$admin_pending_password_file" ] || \
		die "检测到未完成的管理员重置；请重新执行 admin reset 以提交同一密码"

	if has_text "${ICLOUD_API_ADMIN_PASSWORD:-}" && [ "$admin_source" = "environment" ]; then
		# An environment-managed secret is authoritative. Remove any stale file
		# left behind by restoring an archive over a non-empty target volume.
		write_secret "$admin_source_marker" "environment"
		rm -f "$admin_password_file" || die "清理旧管理员密码文件失败"
	elif has_text "${ICLOUD_API_ADMIN_PASSWORD:-}" && \
		[ "${ICLOUD_API_FRESH_BOOTSTRAP:-false}" = "true" ] && \
		[ ! -e "$admin_password_file" ] && [ ! -e "$admin_source_marker" ]; then
		write_secret "$admin_source_marker" "environment"
		admin_source="environment"
	fi
	[ -z "$admin_source" ] || [ ! -e "$admin_password_file" ] || \
		die "管理员密码文件与来源状态冲突；请恢复完整且匹配的 keys 卷"

	if ! has_text "${ICLOUD_API_ADMIN_PASSWORD:-}" && [ "$admin_source" = "environment" ]; then
		die "管理员密码由环境变量管理；请恢复 ICLOUD_API_ADMIN_PASSWORD 或执行 admin reset"
	fi
	if ! has_text "${ICLOUD_API_ADMIN_PASSWORD:-}"; then
		if [ -e "$admin_password_file" ]; then
			ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_password_file" 48)"
		elif [ "$admin_source" = "legacy" ] || \
			{ [ -n "${ICLOUD_API_LEGACY_SQLITE:-}" ] && [ -e "$ICLOUD_API_LEGACY_SQLITE" ]; }; then
			# The startup verifier decides after the read-only SQLite import whether
			# this candidate is needed. It is persisted only for an empty admin table.
			ICLOUD_API_ADMIN_PASSWORD="$(random_hex 24)"
			ICLOUD_API_ADMIN_PASSWORD_PENDING=true
		elif [ "${ICLOUD_API_FRESH_BOOTSTRAP:-false}" = "true" ] || \
			[ -z "${ICLOUD_API_INSTALLATION_ID:-}" ]; then
			candidate="$(random_hex 24)"
			if install_secret_if_absent "$admin_password_file" "$candidate"; then
				ICLOUD_API_ADMIN_PASSWORD="$candidate"
				rm -f "$admin_source_marker"
				printf '%s\n' "icloud-api：已生成管理员密码并保存到 ${admin_password_file}（权限 0600，不写入日志）。" >&2
				printf '%s\n' "icloud-api：查看命令：docker compose exec -T icloud-api cat ${admin_password_file}" >&2
			else
				ICLOUD_API_ADMIN_PASSWORD="$(read_hex_secret "$admin_password_file" 48)"
			fi
		else
			die "管理员密码文件丢失；请恢复 keys 卷，或显式设置 ICLOUD_API_ADMIN_PASSWORD 后执行 admin reset"
		fi
		if [ -n "${ICLOUD_API_ADMIN_PASSWORD:-}" ]; then
			export ICLOUD_API_ADMIN_PASSWORD
		fi
	fi
	if has_text "${ICLOUD_API_OAUTH_TOKEN:-}"; then
		# OAuth is consumed directly on every start, so an explicit value always
		# becomes the durable source and supersedes any generated token file.
		write_secret "$oauth_source_marker" "environment"
		rm -f "$oauth_token_file" || die "清理旧 OAuth Token 文件失败"
		oauth_source="environment"
	fi

	if ! has_text "${ICLOUD_API_OAUTH_TOKEN:-}" && [ "$oauth_source" = "environment" ]; then
		die "OAuth Token 由环境变量管理；请恢复原 ICLOUD_API_OAUTH_TOKEN"
	fi
	[ -z "$oauth_source" ] || [ ! -e "$oauth_token_file" ] || \
		die "OAuth Token 文件与来源状态冲突；请恢复完整且匹配的 keys 卷"

	if ! has_text "${ICLOUD_API_OAUTH_TOKEN:-}"; then
		if [ -e "$oauth_token_file" ]; then
			ICLOUD_API_OAUTH_TOKEN="$(read_hex_secret "$oauth_token_file" 64)"
		elif [ "${ICLOUD_API_FRESH_BOOTSTRAP:-false}" = "true" ] || \
			[ -z "${ICLOUD_API_INSTALLATION_ID:-}" ]; then
			candidate="$(random_hex 32)"
			if install_secret_if_absent "$oauth_token_file" "$candidate"; then
				ICLOUD_API_OAUTH_TOKEN="$candidate"
				rm -f "$oauth_source_marker"
				printf '%s\n' "icloud-api：已生成 OAuth Token 并保存到 ${oauth_token_file}（权限 0600，不写入日志）。" >&2
				printf '%s\n' "icloud-api：查看命令：docker compose exec -T icloud-api cat ${oauth_token_file}" >&2
			else
				ICLOUD_API_OAUTH_TOKEN="$(read_hex_secret "$oauth_token_file" 64)"
			fi
		else
			die "OAuth Token 文件丢失；请恢复 keys 卷或继续提供原 ICLOUD_API_OAUTH_TOKEN"
		fi
		export ICLOUD_API_OAUTH_TOKEN
	fi
}

if [ "${1:-}" = "keygen" ]; then
	exec "${ICLOUD_API_BINARY:-/app/icloud-api}" "$@"
fi

if ! has_text "${ICLOUD_API_DATABASE_URL:-}"; then
	load_database_url
fi
if [ "${1:-}" = "admin" ] && [ "${2:-}" = "reset" ]; then
	acquire_admin_reset_locks
fi
bootstrap_master_key
load_or_generate_application_secrets "$@"
sync || die "持久化应用凭据失败"

if [ "${1:-}" = "admin" ] && [ "${2:-}" = "reset" ]; then
	if "${ICLOUD_API_BINARY:-/app/icloud-api}" "$@"; then
		commit_admin_reset_secret
		commit_key_bootstrap
		exit 0
	else
		status="$?"
		exit "$status"
	fi
fi

if [ "${1:-}" = "verify-startup" ]; then
	exec "${ICLOUD_API_BINARY:-/app/icloud-api}" "$@"
fi

if startup_status="$("${ICLOUD_API_BINARY:-/app/icloud-api}" verify-startup)"; then
	:
else
	status="$?"
	printf '%s\n' "icloud-api 启动失败：应用启动预检失败；主密钥初始化状态保持为待验证" >&2
	exit "$status"
fi
commit_admin_bootstrap_secret "$startup_status"
commit_key_bootstrap
exec "${ICLOUD_API_BINARY:-/app/icloud-api}" "$@"
