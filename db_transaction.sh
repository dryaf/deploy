[[ ${1:?missing operation} == release ]] || fail 'Invalid database operation'
stage=${2:?missing staging path}
request="$stage/request.json"
deployment_mode=$(jq -er .deployment_mode "$request")
case "$deployment_mode" in image|binary) ;; *) fail 'Invalid deployment mode';; esac
init_transaction "$deployment_mode" "$stage"
recover_database_update
[[ ! -f $pending && ! -f $state/boot-pending.json ]] || fail 'Recover the pending deployment before pushing a database'
if service_is_active; then fail 'Service is running; stop it before pushing a database'
else [[ $? == 1 ]] || exit 1; fi
database=$(jq -er .database "$request")
[[ $database == "$target/"* && /${database#"$target/"}/ != */../* && /${database#"$target/"}/ != */.deploy/* ]] || fail 'Unsafe database path'
[[ $(realpath -- "$stage") == "$target/.deploy/db/staging/$id" ]] || fail 'Invalid database staging path'
uid=$(jq -r .container_uid "$request")
gid=$(jq -r .container_gid "$request")
[[ $uid =~ ^[0-9]+$ && $gid =~ ^[0-9]+$ ]] || fail 'Invalid database ownership'
db_check_path "$uid" "$database"
command -v sqlite3 >/dev/null
db_fileop "$uid" test -d "$(dirname -- "$database")" || fail 'Remote database parent directory does not exist'
[[ -f $stage/upload.db && ! -L $stage/upload.db ]] || fail 'Missing staged database'
[[ $(sqlite3 -readonly "$stage/upload.db" 'PRAGMA quick_check;') == ok ]] || fail 'Staged database failed integrity validation'
op="$target/.deploy/db/operations/$id"
make_dir "$op"
chmod 700 "$op"
existed=false checksum='' old_mode=600 old_owner=''
if db_fileop "$uid" test -e "$database"; then
 db_fileop "$uid" test -f "$database" || fail 'Remote database is not a regular file'
 existed=true
 # A SQLite backup includes committed WAL data; copying only the main file does not.
 sqlite_snapshot "$uid" "$database" "$op/before.db"
 retain_database_backup "$database" "$uid" "$op/before.db" "$(jq -r '.environment // "unknown"' "$request")" before-push
 checksum=$(sha256sum -- "$op/before.db" | cut -d' ' -f1)
 old_mode=$(db_fileop "$uid" stat -c '%a' -- "$database")
 old_owner=$(db_fileop "$uid" stat -c '%u:%g' -- "$database")
 db_check_path "$uid" "$database.bak"
 db_check_path "$uid" "$database.bak.deploy-install-$id"
 db_fileop "$uid" cp -- "$op/before.db" "$database.bak.deploy-install-$id"
 db_fileop "$uid" chmod 600 "$database.bak.deploy-install-$id"
 db_fileop "$uid" chown "$old_owner" "$database.bak.deploy-install-$id"
 db_fileop "$uid" sync -f -- "$database.bak.deploy-install-$id"
 db_fileop "$uid" mv -fT -- "$database.bak.deploy-install-$id" "$database.bak"
 db_fileop "$uid" sync -f -- "$(dirname -- "$database")"
fi
temp="$database.deploy-install-$id"
db_check_path "$uid" "$temp"
[[ ! -e $temp && ! -L $temp ]] || fail 'Database temporary path already exists'
db_fileop "$uid" cp -- "$stage/upload.db" "$temp"
db_fileop "$uid" chmod 600 "$temp"
if (( uid > 0 )); then db_fileop "$uid" chown "$uid:$gid" "$temp"; fi
db_fileop "$uid" sync -f -- "$temp"
sync -f -- "$op"
db_pending="$target/.deploy/db/pending.json"
bind_service
jq --argjson existed "$existed" --arg checksum "$checksum" --arg old_mode "$old_mode" --arg old_owner "$old_owner" '. + {existed:$existed,sha256:$checksum,old_mode:$old_mode,old_owner:$old_owner,committed:false}' "$request" | atomic_json "$db_pending"
db_exit() {
 local status=$?
 trap - EXIT HUP INT TERM
 if (( status != 0 )); then
  if ! recover_database_update; then echo "DATABASE ROLLBACK FAILED; pending state and backups preserved at $op" >&2; fi
 fi
 db_fileop "$uid" rm -f -- "$temp" || true
 exit "$status"
}
trap db_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
db_fileop "$uid" rm -f -- "$database-wal" "$database-shm"
db_fileop "$uid" mv -fT -- "$temp" "$database"
db_fileop "$uid" sync -f -- "$(dirname -- "$database")"
jq '.committed=true' "$db_pending" | atomic_json "$db_pending"
durable_remove "$db_pending"
echo 'Database pushed successfully; service remains stopped'
