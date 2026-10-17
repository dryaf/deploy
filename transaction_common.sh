#!/usr/bin/env bash
# Executed remotely as one transaction. No credentials or environment contents are logged.
set -Eeuo pipefail
umask 077

canonical_id() {
 local value=$1
 [[ $value == sha256:* ]] || value="sha256:$value"
 [[ $value =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
 printf '%s\n' "$value"
}

# Called in its own timeout process group so every probe shares the same deadline.
if [[ ${1:-} == health ]]; then
 service=$2 expected=$3 command_check=$4 url=$5
 while true; do
  if timeout --foreground -s KILL 5 systemctl --user is-active --quiet "$service.service"; then
   actual=$(timeout --foreground -s KILL 5 podman container inspect --format '{{.Image}}' "systemd-$service" 2>/dev/null) || actual=''
   actual=$(canonical_id "$actual") || actual=''
   if [[ -z $expected || $actual == "$expected" ]]; then
    ready=true
    if [[ $command_check == true ]]; then timeout --foreground -s KILL 5 podman healthcheck run "systemd-$service" >/dev/null 2>&1 || ready=false; fi
    if [[ -n $url ]]; then timeout --foreground -s KILL 5 curl --fail --silent --show-error --max-time 5 -- "$url" >/dev/null 2>&1 || ready=false; fi
    if $ready; then exit 0; fi
   fi
  fi
  sleep 0.2
 done
fi

fail() { echo "$*" >&2; return 1; }
atomic_json() {
 local destination=$1
 check_path "$destination.tmp" || return
 cat > "$destination.tmp" || return
 chmod 600 "$destination.tmp" || return
 durable_move "$destination.tmp" "$destination"
}
check_path() {
 local p=$1 current=/ component
 local -a components
 [[ $p == /* ]] || { fail 'Expected absolute deployment path'; return 1; }
 IFS=/ read -ra components <<< "$p"
 for component in "${components[@]}"; do
  [[ -n $component ]] || continue
  [[ $component != .. ]] || { fail 'Parent traversal in deployment path'; return 1; }
  current="${current%/}/$component"
  [[ ! -L $current ]] || { fail "Symlink in managed path: $current"; return 1; }
 done
}
make_dir() { check_path "$1" || return; mkdir -p -- "$1"; }

# GNU sync -f invokes syncfs: file data, directory entries and metadata on the
# selected filesystem reach storage before the next transaction phase.
durable_move() {
 sync -f -- "$1" || return
 mv -fT -- "$1" "$2" || return
 sync -f -- "$(dirname -- "$2")"
}
durable_remove() {
 local file
 for file in "$@"; do
  rm -f -- "$file" || return
  sync -f -- "$(dirname -- "$file")" || return
 done
}
sync_image_store() {
 local storage
 storage=$(podman info --format '{{.Store.GraphRoot}}') || return
 [[ $storage == /* && -d $storage ]] || { fail 'Cannot flush Podman image storage'; return 1; }
 sync -f -- "$storage"
}
init_transaction() {
 deployment_mode=$1
 stage=$2
 script=$(realpath -- "$0") || return
 request="$stage/request.json"
 [[ -f $request && ! -L $request ]] || { fail 'Missing release request'; return 1; }
 jq -e '.schema == 1' "$request" >/dev/null || return
 service=$(jq -er .service "$request") || return
 target=$(jq -er .target "$request") || return
 id=$(jq -er .release_id "$request") || return
 [[ $service =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$ && $service != *..* ]] || return 1
 [[ $id =~ ^[a-zA-Z0-9_-]+$ ]] || return 1
 [[ $target == /* && $target != / ]] || return 1
 check_path "$target" || return
 units="${XDG_CONFIG_HOME:-$HOME/.config}/containers/systemd"
 check_path "$units" || return
 unit="$units/$service.container"
 key=$(printf '%s' "$service" | sha256sum | cut -d' ' -f1) || return
 target_key=$(printf '%s' "$target" | sha256sum | cut -d' ' -f1) || return
 locks="$HOME/.local/state/deploy/locks"
 make_dir "$locks" || return
 chmod 700 "$locks" || return
 check_path "$locks/$key.lock" || return
 exec 9>"$locks/$key.lock"
 flock -n 9 || { fail "Deployment lock is busy for $service; retry after the current operation"; return 1; }
 check_path "$locks/target-$target_key.lock" || return
 exec 8>"$locks/target-$target_key.lock"
 flock -n 8 || { fail 'Deployment lock is busy for target_dir; retry after the current operation'; return 1; }
 owner="$locks/$key.owner.json"
 target_owner="$locks/target-$target_key.owner.json"
 check_path "$owner" || return
 check_path "$target_owner" || return
 if [[ -f $owner ]]; then
  jq -e --arg service "$service" --arg target "$target" '.schema == 1 and .service == $service and .target == $target' "$owner" >/dev/null || { fail 'This service belongs to another target_dir; recover it using its original project configuration'; return 1; }
  jq -e --arg mode "$deployment_mode" '(.mode // "image") == $mode' "$owner" >/dev/null || { fail 'Changing deployment mode requires an explicit migration'; return 1; }
 fi
 if [[ -f $target_owner ]]; then
  jq -e --arg service "$service" --arg target "$target" --arg mode "$deployment_mode" '.schema == 1 and .service == $service and .target == $target and .mode == $mode' "$target_owner" >/dev/null || { fail 'target_dir is already owned by another service or deployment mode'; return 1; }
 fi
 state="$target/.deploy/$deployment_mode"
 check_path "$state" || return
 make_dir "$state/releases" || return
 chmod 700 "$state" "$state/releases" || return
 current="$state/current.json"
 pending="$state/pending.json"
 check_path "$current" || return
 check_path "$pending" || return
 # Older installations have no target owner record. Validate their identity before
 # recovery so another service can never restore somebody else's snapshot.
 if [[ -f $current ]]; then
  jq -e --arg service "$service" --arg target "$target" '.service == $service and .target == $target' "$current" >/dev/null || { fail 'target_dir contains another service deployment'; return 1; }
 fi
 if [[ -f $pending ]]; then
  local pending_id
  pending_id=$(jq -er .release_id "$pending") || return
  [[ $pending_id =~ ^[a-zA-Z0-9_-]+$ ]] || return 1
  check_path "$state/releases/$pending_id/manifest.json" || return
  jq -e --arg service "$service" --arg target "$target" '.service == $service and .target == $target' "$state/releases/$pending_id/manifest.json" >/dev/null || { fail 'Pending deployment belongs to another service'; return 1; }
 fi
}

# Database updates share deployment locks. Recovery leaves the service stopped,
# matching the mandatory precondition for publishing a database replacement.
db_fileop() {
 local uid=$1
 shift
 if (( uid > 0 )); then podman unshare "$@"; else "$@"; fi
}
db_check_path() {
 local uid=$1 path=$2
 if (( uid > 0 )); then
  # Check as the same namespace user that performs file operations. A container
  # owned directory may be unreadable to the SSH user and conceal a symlink.
  podman unshare bash -c "$(declare -f fail check_path); check_path \"\$1\"" -- "$path"
 else check_path "$path"; fi
}
recover_database_update() {
 local journal="$target/.deploy/db/pending.json" metadata op database uid existed temp committed
 [[ -f $journal ]] || return 0
 check_path "$journal" || return
 metadata=$(cat -- "$journal") || return
 jq -e --arg service "$service" --arg target "$target" '.schema == 1 and .service == $service and .target == $target and (.release_id | test("^[a-zA-Z0-9_-]+$")) and (.database | type == "string") and (.existed | type == "boolean") and (.committed | type == "boolean") and (.container_uid | type == "number")' <<< "$metadata" >/dev/null || return
 committed=$(jq -r .committed <<< "$metadata") || return
 if [[ $committed == true ]]; then durable_remove "$journal"; return; fi
 op="$target/.deploy/db/operations/$(jq -r .release_id <<< "$metadata")"
 database=$(jq -er .database <<< "$metadata") || return
 [[ $database == "$target/"* && /${database#"$target/"}/ != */../* && /${database#"$target/"}/ != */.deploy/* ]] || return 1
 uid=$(jq -r .container_uid <<< "$metadata") || return
 existed=$(jq -r .existed <<< "$metadata") || return
 check_path "$op" || return
 db_check_path "$uid" "$database" || return
 temp="$database.deploy-restore-$(jq -r .release_id <<< "$metadata")"
 db_check_path "$uid" "$temp" || return
 if [[ $existed == true ]]; then
  [[ $(sha256sum -- "$op/before.db" | cut -d' ' -f1) == $(jq -r .sha256 <<< "$metadata") ]] || { fail 'Database backup checksum mismatch'; return 1; }
 fi
 if service_is_active; then systemctl --user stop "$service.service" || return
 else [[ $? == 1 ]] || return 1; fi
 db_fileop "$uid" rm -f -- "$database-wal" "$database-shm" || return
 if [[ $existed == true ]]; then
  db_fileop "$uid" cp -- "$op/before.db" "$temp" || return
  db_fileop "$uid" chmod "$(jq -r .old_mode <<< "$metadata")" "$temp" || return
  db_fileop "$uid" chown "$(jq -r .old_owner <<< "$metadata")" "$temp" || return
  db_fileop "$uid" sync -f -- "$temp" || return
  db_fileop "$uid" mv -fT -- "$temp" "$database" || return
 else db_fileop "$uid" rm -f -- "$database" || return; fi
 db_fileop "$uid" sync -f -- "$(dirname -- "$database")" || return
 durable_remove "$journal" || return
 echo 'Previous database restored; service remains stopped' >&2
}
bind_service() {
 local record
 record=$(jq -n --arg service "$service" --arg target "$target" --arg mode "$deployment_mode" '{schema:1,service:$service,target:$target,mode:$mode}') || return
 atomic_json "$owner" <<< "$record" || return
 atomic_json "$target_owner" <<< "$record"
}
service_is_active() {
 local status
 if systemctl --user is-active --quiet "$service.service"; then return 0; else status=$?; fi
 case "$status" in 3|4) return 1;; *) fail 'Cannot determine service activity'; return 2;; esac
}
wait_health() {
 local manifest=$1 image=$2
 local seconds command_check url
 seconds=$(jq -er .health_seconds "$manifest")
 command_check=$(jq -r .health_command "$manifest")
 url=$(jq -r '.health_url // ""' "$manifest")
 timeout -s KILL "$seconds" bash "$script" health "$service" "$image" "$command_check" "$url" 8>&- 9>&- >/dev/null 2>&1
}
record_path() {
 local rel=$1 kind=$2 dest="$target/$1" existed=false mode_value='' checksum=''
 [[ $rel != /* && /$rel/ != */../* && /$rel/ != */.deploy/* ]] || { fail "Unsafe managed artifact: $rel"; return 1; }
 check_path "$dest"
 [[ $dest != "$units" && $dest != "$units/"* && $dest != "$locks" && $dest != "$locks/"* ]] || fail 'Artifact overlaps deployment control files'
 [[ ! -e $dest.deploy-install-$id && ! -L $dest.deploy-install-$id ]] || fail 'Reserved deployment temporary path already exists'
 if [[ -e $dest ]]; then
  existed=true
  if [[ $kind == directory ]]; then [[ -d $dest ]] || fail 'Artifact directory conflicts with existing file'
  else [[ -f $dest ]] || fail 'Artifact file conflicts with existing directory'; fi
  mode_value=$(stat -c '%a' -- "$dest")
  if [[ $kind == file ]]; then mkdir -p -- "$before/files/$(dirname -- "$rel")"; cp -p -- "$dest" "$before/files/$rel"; chmod 600 "$before/files/$rel"; checksum=$(sha256sum -- "$before/files/$rel" | cut -d' ' -f1); fi
 fi
 jq --arg rel "$rel" --arg kind "$kind" --arg mode "$mode_value" --arg checksum "$checksum" --argjson existed "$existed" '. + [{path:$rel,kind:$kind,mode:$mode,existed:$existed,sha256:$checksum}]' "$before/files.json" | atomic_json "$before/files.json"
}
validate_managed_snapshot() {
 local before=$1 entry rel
 jq -e 'type == "array" and all(.[]; (.path | type == "string") and (.path | startswith("/") | not) and (.path | split("/") | all(. != ".." and . != ".deploy" and . != "")) and (.existed | type == "boolean") and (.kind == "file" or .kind == "directory"))' "$before/files.json" >/dev/null || return
 jq -c '.[]' "$before/files.json" > "$before/restore-entries.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  if [[ $(jq -r .existed <<< "$entry") == true && $(jq -r .kind <<< "$entry") == file ]]; then
   check_path "$before/files/$rel" || return
   [[ -f $before/files/$rel ]] || { fail 'Rollback file backup missing'; return 1; }
   [[ $(sha256sum -- "$before/files/$rel" | cut -d' ' -f1) == $(jq -r .sha256 <<< "$entry") ]] || { fail 'Rollback file backup checksum mismatch'; return 1; }
  fi
 done < "$before/restore-entries.jsonl"
}
prepare_managed_directories() {
 local entries=$1 entry rel previous_mode
 while IFS= read -r entry; do
  [[ $(jq -r .kind <<< "$entry") == directory ]] || continue
  rel=$(jq -r .path <<< "$entry") || return
  check_path "$target/$rel" || return
  if [[ ! -d $target/$rel ]]; then
   mkdir -p -- "$target/$rel" || return
   changed=true
  fi
  previous_mode=$(stat -c '%a' -- "$target/$rel") || return
  chmod u+rwx -- "$target/$rel" || return
  if [[ $(stat -c '%a' -- "$target/$rel") != "$previous_mode" ]]; then changed=true; fi
 done < "$entries"
}
install_managed_files() {
 local before=$1 payload=$2 entry rel kind source
 jq -c '.[]' "$before/files.json" > "$release/install-entries.jsonl" || return
 prepare_managed_directories "$release/install-entries.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  kind=$(jq -r .kind <<< "$entry") || return
  [[ $kind != directory ]] || continue
  check_path "$target/$rel" || return
  if [[ $rel == .env && $(jq -r .sync_env "$request") == true ]]; then source="$stage/env"; else source="$payload/$rel"; fi
  check_path "$target/$rel.deploy-install-$id" || return
  cp -p -- "$source" "$target/$rel.deploy-install-$id" || return
  if [[ $rel == .env ]]; then chmod 600 "$target/$rel.deploy-install-$id" || return; fi
  durable_move "$target/$rel.deploy-install-$id" "$target/$rel" || return
  changed=true
 done < "$release/install-entries.jsonl"
 # Apply restrictive directory permissions only after installing all descendants.
 jq -c 'reverse[] | select(.kind == "directory")' "$before/files.json" > "$release/directory-modes.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  chmod --reference="$payload/$rel" -- "$target/$rel" || return
  sync -f -- "$target/$rel" || return
 done < "$release/directory-modes.jsonl"
 sync -f -- "$target"
}
restore_managed_files() {
 local before=$1 restore_id=$2 entry rel existed kind mode_value
 prepare_managed_directories "$before/restore-entries.jsonl" || return
 jq -c 'reverse[]' "$before/files.json" > "$before/restore-reversed.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  kind=$(jq -r .kind <<< "$entry") || return
  [[ $kind != directory ]] || continue
  existed=$(jq -r .existed <<< "$entry") || return
  mode_value=$(jq -r .mode <<< "$entry") || return
  check_path "$target/$rel" || return
  check_path "$target/$rel.deploy-install-$restore_id" || return
  rm -f -- "$target/$rel.deploy-install-$restore_id" || return
  if [[ $existed == true ]]; then
   check_path "$target/$rel.deploy-restore-$restore_id" || return
   cp -p -- "$before/files/$rel" "$target/$rel.deploy-restore-$restore_id" || return
   chmod "$mode_value" "$target/$rel.deploy-restore-$restore_id" || return
   durable_move "$target/$rel.deploy-restore-$restore_id" "$target/$rel" || return
  else durable_remove "$target/$rel" || return; fi
 done < "$before/restore-reversed.jsonl"
 while IFS= read -r entry; do
  [[ $(jq -r .kind <<< "$entry") == directory ]] || continue
  rel=$(jq -r .path <<< "$entry") || return
  if [[ $(jq -r .existed <<< "$entry") == true ]]; then
   chmod "$(jq -r .mode <<< "$entry")" -- "$target/$rel" || return
  else
   # Preserve unrelated files created by the application or another actor.
   rmdir -- "$target/$rel" 2>/dev/null || { [[ -d $target/$rel ]] || return 1; }
  fi
  sync -f -- "$(dirname -- "$target/$rel")" || return
 done < "$before/restore-reversed.jsonl"
 sync -f -- "$target"
}

snapshot_unchanged() {
 local release=$1 before="$1/before" expected_active current_active=false entry rel kind existed
 [[ -f $before/snapshot.json && -f $before/files.json ]] || return 1
 if [[ $(jq -r .quadlet "$before/snapshot.json") == true ]]; then
  cmp -s "$unit" "$before/active.container" || return 1
 else [[ ! -e $unit ]] || return 1; fi
 if [[ -f $before/current.json ]]; then cmp -s "$current" "$before/current.json" || return 1
 else [[ ! -e $current ]] || return 1; fi
 expected_active=$(jq -r .active "$before/snapshot.json") || return
 if service_is_active; then current_active=true; else [[ $? == 1 ]] || return 1; fi
 [[ $expected_active == "$current_active" ]] || return 1
 jq -c '.[]' "$before/files.json" > "$before/unchanged-entries.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  check_path "$target/$rel" || return
  existed=$(jq -r .existed <<< "$entry") || return
  kind=$(jq -r .kind <<< "$entry") || return
  if [[ $existed == true ]]; then
   [[ $(stat -c '%a' -- "$target/$rel") == $(jq -r .mode <<< "$entry") ]] || return 1
   if [[ $kind == file ]]; then cmp -s "$target/$rel" "$before/files/$rel" || return 1
   else [[ -d $target/$rel ]] || return 1; fi
  else [[ ! -e $target/$rel ]] || return 1; fi
 done < "$before/unchanged-entries.jsonl"
}
discard_prepared() {
 local release=$1 release_id entry rel
 release_id=${release##*/}
 check_path "$unit.deploy-install-$release_id" || return
 rm -f -- "$unit.deploy-install-$release_id" || return
 jq -c '.[]' "$release/before/files.json" > "$release/before/cleanup-entries.jsonl" || return
 while IFS= read -r entry; do
  rel=$(jq -r .path <<< "$entry") || return
  check_path "$target/$rel.deploy-install-$release_id" || return
  rm -f -- "$target/$rel.deploy-install-$release_id" || return
 done < "$release/before/cleanup-entries.jsonl"
 durable_remove "$pending" || return
 if [[ ! -f $current ]]; then durable_remove "$owner" "$target_owner" || return; fi
}


init_boot_state() {
boot_record="$state/boot.json"
boot_pending="$state/boot-pending.json"
boot_dir="$units/$service.container.d"
boot_file="$boot_dir/90-deploy-boot.conf"
}
boot_content() {
 printf '[Install]\nWantedBy=\n'
 if [[ $1 == true ]]; then printf 'WantedBy=default.target\n'; fi
}
validate_boot() {
 check_path "$boot_record"; check_path "$boot_file"
 if [[ -f $boot_record ]]; then
  jq -e '.schema == 1 and (.enabled | type == "boolean")' "$boot_record" >/dev/null
  [[ -f $boot_file ]] || fail 'Boot configuration is missing'
  boot_content "$(jq -r .enabled "$boot_record")" > "$stage/expected-boot.conf"
  cmp -s "$stage/expected-boot.conf" "$boot_file" || fail 'Boot configuration differs from managed state'
 else [[ ! -e $boot_file ]] || fail 'Unmanaged boot override cannot be adopted'; fi
}
restore_boot() {
 local operation_id backup
 operation_id=$(jq -er .operation_id "$boot_pending") || return
 [[ $operation_id =~ ^[a-zA-Z0-9_-]+$ ]] || return 1
 backup="$state/operations/$operation_id"
 check_path "$backup" || return
 check_path "$boot_file" || return
 if [[ $(jq -r .file_existed "$boot_pending") == true ]]; then
  [[ -f $backup/boot.conf ]] || return 1
  cat "$backup/boot.conf" | atomic_json "$boot_file" || return
 else durable_remove "$boot_file" || return; fi
 if [[ $(jq -r .record_existed "$boot_pending") == true ]]; then
  [[ -f $backup/boot.json ]] || return 1
  cat "$backup/boot.json" | atomic_json "$boot_record" || return
 else durable_remove "$boot_record" || return; fi
 systemctl --user daemon-reload || return
 durable_remove "$boot_pending" || return
}
set_boot() {
 local enabled=$1 backup="$state/operations/$id" file_existed=false record_existed=false
 validate_boot
 make_dir "$backup"
 if [[ -f $boot_file ]]; then cp -p -- "$boot_file" "$backup/boot.conf"; file_existed=true; fi
 if [[ -f $boot_record ]]; then cp -p -- "$boot_record" "$backup/boot.json"; record_existed=true; fi
 jq -n --arg id "$id" --argjson file "$file_existed" --argjson record "$record_existed" '{schema:1,operation_id:$id,file_existed:$file,record_existed:$record}' | atomic_json "$boot_pending"
 trap 'boot_status=$?; trap - EXIT HUP INT TERM; if (( boot_status != 0 )); then if ! restore_boot; then echo "Boot configuration rollback failed; boot-pending.json retained" >&2; fi; fi; exit "$boot_status"' EXIT
 trap 'exit 129' HUP
 trap 'exit 130' INT
 trap 'exit 143' TERM
 make_dir "$boot_dir"
 boot_content "$enabled" | atomic_json "$boot_file"
 jq -n --argjson enabled "$enabled" '{schema:1,enabled:$enabled}' | atomic_json "$boot_record"
 systemctl --user daemon-reload
 durable_remove "$boot_pending"
}
