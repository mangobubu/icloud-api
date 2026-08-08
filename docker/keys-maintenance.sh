#!/bin/sh
set -eu
umask 077

die() {
	printf '%s\n' "icloud-api keys 维护失败：$*" >&2
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

random_hex() {
	byte_count="$1"
	value="$(od -An -N "$byte_count" -tx1 /dev/urandom | tr -d ' \n')"
	[ "${#value}" -eq "$((byte_count * 2))" ] || die "生成恢复哨兵失败"
	printf '%s' "$value"
}

is_known_key_name() {
	case "$1" in
		master.key|admin-password|admin-password.pending|admin-password.source|oauth-token|oauth-token.source)
			return 0
			;;
		*)
			return 1
			;;
	esac
}

validate_directory_entries() {
	directory="$1"
	for candidate in "$directory"/* "$directory"/.[!.]* "$directory"/..?*; do
		if [ ! -e "$candidate" ] && [ ! -L "$candidate" ]; then
			continue
		fi
		name="${candidate##*/}"
		is_known_key_name "$name" || die "密钥目录包含意外项目：$name"
		[ -f "$candidate" ] && [ ! -L "$candidate" ] || die "$name 不是普通文件"
		[ "$(stat -c '%h' "$candidate")" = "1" ] || die "$name 不能是硬链接"
	done
}

validate_hex_file() {
	file="$1"
	expected_length="$2"
	name="${file##*/}"
	[ -f "$file" ] && [ ! -L "$file" ] || die "$name 不是普通文件"
	[ "$(wc -l < "$file" | tr -d ' ')" = "1" ] || die "$name 格式错误"
	[ "$(wc -c < "$file" | tr -d ' ')" -eq "$((expected_length + 1))" ] || die "$name 格式错误"
	value="$(sed -n '1p' "$file")"
	[ "${#value}" -eq "$expected_length" ] || die "$name 长度错误"
	case "$value" in
		*[!0-9a-f]*) die "$name 包含非法字符" ;;
	esac
}

validate_source_file() {
	file="$1"
	type="$2"
	name="${file##*/}"
	[ -f "$file" ] && [ ! -L "$file" ] || die "$name 不是普通文件"
	[ "$(wc -l < "$file" | tr -d ' ')" = "1" ] || die "$name 格式错误"
	source_value="$(sed -n '1p' "$file")"
	[ "$(wc -c < "$file" | tr -d ' ')" -eq "$(( $(byte_length "$source_value") + 1 ))" ] || \
		die "$name 格式错误"
	case "$type:$source_value" in
		admin:legacy|admin:environment|oauth:environment) ;;
		*) die "$name 格式错误" ;;
	esac
}

validate_master_key_value() {
	master_key_value="$1"
	master_key_is_hex=false
	case "$master_key_value" in
		*[!0-9A-Fa-f]*) ;;
		*) [ "${#master_key_value}" -eq 64 ] && master_key_is_hex=true ;;
	esac
	[ "$master_key_is_hex" = false ] || return 0

	case "$master_key_value" in
		*=*)
			case "$master_key_value" in
				*[!A-Za-z0-9+/=]*) die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值" ;;
			esac
			[ "$(( ${#master_key_value} % 4 ))" -eq 0 ] || \
				die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值"
			normalized_master_key="$master_key_value"
			;;
		*[-_]*)
			case "$master_key_value" in
				*[!A-Za-z0-9_-]*) die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值" ;;
			esac
			normalized_master_key="$(printf '%s' "$master_key_value" | tr '_-' '/+')"
			;;
		*)
			case "$master_key_value" in
				*[!A-Za-z0-9+/]*) die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值" ;;
			esac
			normalized_master_key="$master_key_value"
			;;
	esac

	case "$master_key_value" in
		*=*) ;;
		*)
			case "$(( ${#normalized_master_key} % 4 ))" in
				0) ;;
				2) normalized_master_key="${normalized_master_key}==" ;;
				3) normalized_master_key="${normalized_master_key}=" ;;
				*) die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值" ;;
			esac
			;;
	esac

	decoded_master_key="$(mktemp "${TMPDIR:-/tmp}/icloud-api-master-key.XXXXXX")" || \
		die "创建主密钥校验临时文件失败"
	if ! printf '%s' "$normalized_master_key" | base64 -d > "$decoded_master_key" 2>/dev/null; then
		rm -f "$decoded_master_key"
		die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值"
	fi
	decoded_length="$(wc -c < "$decoded_master_key" | tr -d ' ')"
	rm -f "$decoded_master_key"
	[ "$decoded_length" -eq 32 ] || die "主密钥不是 Go 支持的 32 字节 Base64 或十六进制值"
}

validate_admin_environment() {
	admin_value="${ICLOUD_API_ADMIN_PASSWORD:-}"
	has_text "$admin_value" || die "admin-password.source=environment，但未恢复 ICLOUD_API_ADMIN_PASSWORD"
	admin_length="$(byte_length "$admin_value")"
	[ "$admin_length" -ge 12 ] && [ "$admin_length" -le 72 ] || \
		die "ICLOUD_API_ADMIN_PASSWORD 必须为 12 到 72 字节"
}

validate_oauth_environment() {
	oauth_value="${ICLOUD_API_OAUTH_TOKEN:-}"
	oauth_length="$(byte_length "$oauth_value")"
	[ "$oauth_length" -ge 32 ] && [ "$oauth_length" -le 4096 ] || \
		die "ICLOUD_API_OAUTH_TOKEN 必须为 32 到 4096 个不含空白的字符"
	case "$oauth_value" in
		*[[:space:]]*) die "ICLOUD_API_OAUTH_TOKEN 必须为 32 到 4096 个不含空白的字符" ;;
	esac
}

validate_key_set() {
	directory="$1"
	allow_pending_admin_without_environment="${2:-false}"
	validate_directory_entries "$directory"

	if [ -f "$directory/master.key" ]; then
		master_file_value="$(cat "$directory/master.key")"
		[ "$(wc -l < "$directory/master.key" | tr -d ' ')" = "1" ] && \
			[ "$(wc -c < "$directory/master.key" | tr -d ' ')" -eq \
			"$(( $(byte_length "$master_file_value") + 1 ))" ] || die "master.key 格式错误"
		validate_master_key_value "$master_file_value"
	fi
	if has_text "${ICLOUD_API_MASTER_KEY:-}"; then
		validate_master_key_value "$ICLOUD_API_MASTER_KEY"
	elif [ ! -f "$directory/master.key" ]; then
		die "密钥集没有 master.key；请先恢复同一恢复集的 ICLOUD_API_MASTER_KEY"
	fi

	admin_password_present=false
	admin_pending_present=false
	admin_source_present=false
	[ ! -f "$directory/admin-password" ] || admin_password_present=true
	[ ! -f "$directory/admin-password.pending" ] || admin_pending_present=true
	[ ! -f "$directory/admin-password.source" ] || admin_source_present=true
	if [ "$admin_password_present" = true ] && [ "$admin_source_present" = true ]; then
		die "管理员密码与来源标记不能同时存在"
	fi
	[ "$admin_password_present" = true ] || [ "$admin_source_present" = true ] || \
		[ "$admin_pending_present" = true ] || die "密钥集缺少管理员凭据"
	[ "$admin_password_present" = false ] || validate_hex_file "$directory/admin-password" 48
	[ "$admin_pending_present" = false ] || validate_hex_file "$directory/admin-password.pending" 48
	if [ "$admin_source_present" = true ]; then
		validate_source_file "$directory/admin-password.source" admin
		if [ "$source_value" = environment ] && \
			{ [ "$allow_pending_admin_without_environment" = false ] || \
			[ "$admin_pending_present" = false ]; }; then
			validate_admin_environment
		fi
	fi

	if [ -f "$directory/oauth-token" ] && [ -f "$directory/oauth-token.source" ]; then
		die "OAuth Token 与来源标记不能同时存在"
	elif [ -f "$directory/oauth-token" ]; then
		validate_hex_file "$directory/oauth-token" 64
	elif [ -f "$directory/oauth-token.source" ]; then
		validate_source_file "$directory/oauth-token.source" oauth
		validate_oauth_environment
	else
		die "密钥集缺少 OAuth Token 或来源标记"
	fi
}

backup_keys() {
	validate_key_set "$keys_directory" false
	set --
	for name in $known_names; do
		[ ! -f "$keys_directory/$name" ] || set -- "$@" "$name"
	done
	[ "$#" -gt 0 ] || die "密钥集为空"
	exec tar -C "$keys_directory" -cf - "$@"
}

archive_entry_name() {
	entry="$1"
	case "$entry" in
		.|./) printf '%s' "" ;;
		./*) printf '%s' "${entry#./}" ;;
		*) printf '%s' "$entry" ;;
	esac
}

restore_keys() {
	archive="$1"
	validate_only="${2:-false}"
	if [ "$archive" != "-" ]; then
		[ -s "$archive" ] || die "应用密钥归档为空或不存在"
	fi

	archive_copy=""
	stage=""
	old=""
	listing=""
	publish_started=false
	publish_committed=false
	rollback_preserved=false
	cleanup_restore() {
		status="$?"
		trap - EXIT
		trap '' HUP INT TERM
		set +e
		cleanup_failed=false
		if [ "$publish_started" = true ] && [ "$publish_committed" = false ]; then
			rollback_failed=false
			for name in master.key admin-password admin-password.source oauth-token oauth-token.source; do
				if [ -f "$old/$name" ]; then
					cp -p "$old/$name" "$keys_directory/$name" || rollback_failed=true
				else
					rm -f "$keys_directory/$name" || rollback_failed=true
				fi
			done
			if [ "$rollback_failed" = false ]; then
				sync || rollback_failed=true
			fi
			if [ "$rollback_failed" = false ]; then
				if [ -f "$old/admin-password.pending" ]; then
					cp -p "$old/admin-password.pending" \
						"$keys_directory/admin-password.pending" || rollback_failed=true
				else
					rm -f "$keys_directory/admin-password.pending" || rollback_failed=true
				fi
			fi
			if [ "$rollback_failed" = false ]; then
				sync || rollback_failed=true
			fi
			if [ "$rollback_failed" = true ]; then
				rollback_preserved=true
				printf '%s\n' "密钥回滚未完整完成；回滚副本保留在 $old，请勿启动应用" >&2
			fi
		fi
		[ -z "$stage" ] || rm -rf "$stage" || cleanup_failed=true
		[ -z "$listing" ] || rm -f "$listing" || cleanup_failed=true
		[ -z "$archive_copy" ] || rm -f "$archive_copy" || cleanup_failed=true
		if [ -n "$old" ] && [ "$rollback_preserved" = false ]; then
			rm -rf "$old" || cleanup_failed=true
		fi
		sync || cleanup_failed=true
		if [ "$cleanup_failed" = true ]; then
			printf '%s\n' "密钥维护临时文件清理未完整完成；请勿启动应用" >&2
			[ "$status" -ne 0 ] || status=1
		fi
		exit "$status"
	}
	trap cleanup_restore EXIT
	trap 'exit 1' HUP INT TERM

	for stale_old in "$keys_directory"/.restore-old.*; do
		if [ ! -e "$stale_old" ] && [ ! -L "$stale_old" ]; then
			continue
		fi
		die "发现上次恢复保留的回滚副本：$stale_old；请先核对并移出 keys 卷"
	done

	archive_copy="$(mktemp "${TMPDIR:-/tmp}/icloud-api-keys-archive.XXXXXX")" || \
		die "创建密钥归档临时文件失败"
	if [ "$archive" = "-" ]; then
		if ! cat > "$archive_copy"; then
			die "从标准输入读取应用密钥归档失败"
		fi
	elif ! cat < "$archive" > "$archive_copy"; then
		die "读取应用密钥归档失败"
	fi
	[ -s "$archive_copy" ] || die "应用密钥归档为空"
	stage="$(mktemp -d "$keys_directory/.restore-stage.XXXXXX")" || die "创建密钥恢复临时目录失败"
	listing="$(mktemp "${TMPDIR:-/tmp}/icloud-api-keys-list.XXXXXX")" || die "创建归档目录临时文件失败"
	if ! tar -tf "$archive_copy" > "$listing"; then
		die "应用密钥归档不是有效的 tar 文件"
	fi
	seen_names=" "
	while IFS= read -r archive_entry; do
		name="$(archive_entry_name "$archive_entry")"
		[ -n "$name" ] || continue
		is_known_key_name "$name" || die "应用密钥归档包含意外路径：$archive_entry"
		case "$seen_names" in
			*" $name "*) die "应用密钥归档重复包含：$name" ;;
		esac
		seen_names="${seen_names}${name} "
	done < "$listing"
	if ! tar -C "$stage" -xf "$archive_copy"; then
		die "解包应用密钥归档失败"
	fi
	validate_key_set "$stage" true
	for name in $known_names; do
		[ ! -f "$stage/$name" ] || chmod 600 "$stage/$name" || die "设置 $name 权限失败"
	done
	if [ "$validate_only" = true ]; then
		printf '%s\n' "应用密钥归档校验通过。" >&2
		return 0
	fi

	for stale in "$keys_directory"/.restore-stage.*; do
		if [ ! -e "$stale" ] && [ ! -L "$stale" ]; then
			continue
		fi
		[ "$stale" = "$stage" ] || rm -rf "$stale" || die "清理旧恢复临时目录失败"
	done
	old="$(mktemp -d "$keys_directory/.restore-old.XXXXXX")" || die "创建密钥回滚目录失败"
	for name in $known_names; do
		target="$keys_directory/$name"
		if [ -e "$target" ] || [ -L "$target" ]; then
			[ -f "$target" ] && [ ! -L "$target" ] || die "现有 $name 不是普通文件"
			cp -p "$target" "$old/$name" || die "备份现有 $name 失败"
		fi
	done
	sync || die "持久化应用密钥回滚副本失败"

	archive_has_master=false
	archive_has_admin_pending=false
	[ ! -f "$stage/master.key" ] || archive_has_master=true
	[ ! -f "$stage/admin-password.pending" ] || archive_has_admin_pending=true
	if [ "$archive_has_admin_pending" = true ]; then
		app_state_directory="${ICLOUD_API_INSTALLATION_STATE_DIR:-/run/icloud-api-installation}/app-state"
		maintenance_lock_file="${app_state_directory}/maintenance.lock"
		maintenance_window_lock_file="${app_state_directory}/maintenance-window.lock"
		[ -d "$app_state_directory" ] && [ ! -L "$app_state_directory" ] || \
			die "安装状态目录不存在或不是普通目录：$app_state_directory"
		if [ -e "$maintenance_lock_file" ] || [ -L "$maintenance_lock_file" ]; then
			[ -f "$maintenance_lock_file" ] && [ ! -L "$maintenance_lock_file" ] || \
				die "维护锁不是普通文件：$maintenance_lock_file"
		fi
		[ -f "$maintenance_window_lock_file" ] && [ ! -L "$maintenance_window_lock_file" ] || \
			die "维护窗口上下文锁不存在或不是普通文件"
		exec 7>"$maintenance_window_lock_file" || die "打开维护窗口上下文锁失败"
		if flock -n 7; then
			flock -u 7 || true
			die "包含待提交管理员密码的恢复缺少宿主维护窗口上下文"
		fi
		exec 8>"$maintenance_lock_file" || die "打开宿主维护锁失败"
		if flock -n 8; then
			flock -u 8 || true
			die "包含待提交管理员密码的恢复必须位于宿主维护窗口内"
		fi
	fi
	if [ "$archive_has_admin_pending" = false ]; then
		if [ -f "$stage/admin-password" ]; then
			cp "$stage/admin-password" "$stage/.restore-pending" || die "创建恢复哨兵失败"
		else
			restore_sentinel="$(random_hex 24)" || die "生成恢复哨兵失败"
			printf '%s\n' "$restore_sentinel" > "$stage/.restore-pending" || die "创建恢复哨兵失败"
		fi
		chmod 600 "$stage/.restore-pending" || die "设置恢复哨兵权限失败"
	fi

	publish_started=true
	if [ "$archive_has_admin_pending" = true ]; then
		mv -f "$stage/admin-password.pending" "$keys_directory/admin-password.pending"
	else
		mv -f "$stage/.restore-pending" "$keys_directory/admin-password.pending"
	fi
	sync || die "持久化应用密钥恢复阻断标记失败"
	if [ "$archive_has_master" = true ]; then
		mv -f "$stage/master.key" "$keys_directory/master.key"
	fi
	if [ -f "$stage/admin-password" ]; then
		mv -f "$stage/admin-password" "$keys_directory/admin-password"
		rm -f "$keys_directory/admin-password.source"
	elif [ -f "$stage/admin-password.source" ]; then
		mv -f "$stage/admin-password.source" "$keys_directory/admin-password.source"
		rm -f "$keys_directory/admin-password"
	else
		rm -f "$keys_directory/admin-password" "$keys_directory/admin-password.source"
	fi
	if [ -f "$stage/oauth-token" ]; then
		mv -f "$stage/oauth-token" "$keys_directory/oauth-token"
		rm -f "$keys_directory/oauth-token.source"
	else
		mv -f "$stage/oauth-token.source" "$keys_directory/oauth-token.source"
		rm -f "$keys_directory/oauth-token"
	fi
	[ "$archive_has_master" = true ] || rm -f "$keys_directory/master.key"
	sync || die "持久化恢复后的应用密钥失败"
	publish_committed=true
	if [ "$archive_has_admin_pending" = false ]; then
		rm -f "$keys_directory/admin-password.pending" || \
			die "清理恢复哨兵失败；请保持应用停止并重新执行 restore"
		sync || die "持久化应用密钥恢复完成状态失败"
	fi

	if [ "$archive_has_admin_pending" = true ]; then
		if ! env -u ICLOUD_API_ADMIN_PASSWORD \
			ICLOUD_API_ADMIN_RESET_LOCK_CONTEXT=keys-maintenance-restore-v1 \
			"${ICLOUD_API_ENTRYPOINT:-/usr/local/bin/icloud-api-entrypoint}" admin reset; then
			die "密钥已恢复，但未完成的管理员重置提交失败；请保持应用停止并重新执行 restore"
		fi
		[ ! -e "$keys_directory/admin-password.pending" ] || \
			die "管理员重置返回成功但待提交密码仍存在；请保持应用停止"
		[ ! -e "$keys_directory/admin-password.source" ] || \
			die "管理员重置返回成功但旧密码来源仍存在；请保持应用停止"
		validate_hex_file "$keys_directory/admin-password" 48
	fi
	printf '%s\n' "应用密钥恢复完成。" >&2
}

keys_directory="${ICLOUD_API_KEYS_DIR:-/app/keys}"
known_names="master.key admin-password admin-password.pending admin-password.source oauth-token oauth-token.source"
[ -d "$keys_directory" ] && [ ! -L "$keys_directory" ] || die "密钥目录不存在或不是普通目录"
exec 9<"$keys_directory" || die "打开密钥卷互斥锁失败"
flock -n 9 || die "另一个密钥备份、校验或恢复任务正在运行"

case "${1:-}" in
	backup)
		[ "$#" -eq 1 ] || die "backup 不接受额外参数"
		backup_keys
		;;
	restore)
		[ "$#" -eq 2 ] || die "restore 需要一个 tar 归档路径或 -"
		restore_keys "$2" false
		;;
	validate)
		[ "$#" -eq 2 ] || die "validate 需要一个 tar 归档路径或 -"
		restore_keys "$2" true
		;;
	*)
		die "用法：keys-maintenance.sh backup | validate ARCHIVE|- | restore ARCHIVE|-"
		;;
esac
