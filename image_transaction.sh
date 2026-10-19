mode=${1:?missing operation}
stage=${2:?missing staging path}
init_transaction image "$stage"
store_lock -s
if [[ -f $target/.deploy/db/pending.json ]]; then bounded_recovery recover_database_update; fi

init_boot_state

validate_commit() {
 jq -e --arg service "$service" --arg target "$target" '.schema == 1 and .service == $service and .target == $target and (.current | test("^[a-zA-Z0-9_-]+$")) and (.previous == null or (.previous | test("^[a-zA-Z0-9_-]+$")))' "$current" >/dev/null
}

# Return errors explicitly: rollback is deliberately called in a conditional, where
# Bash errexit does not propagate. Never rely on errexit inside this function.
restore_image_deployment() {
 local release=$1 before="$1/before" rel existed kind file saved mode_value image active restore_id
 restore_id=${release##*/}
 [[ -f $before/snapshot.json && -f $before/files.json ]] || { fail 'Incomplete rollback snapshot'; return 1; }
 check_path "$unit" || return
 jq -e '(.active | type == "boolean") and (.quadlet | type == "boolean") and (.image_id | type == "string")' "$before/snapshot.json" >/dev/null || return
 validate_managed_snapshot "$before" || return
 image=$(jq -r '.image_id // ""' "$before/snapshot.json") || return
 active=$(jq -r .active "$before/snapshot.json") || return
 if [[ -n $image ]]; then
  canonical_id "$image" >/dev/null || return
  [[ -f $before/active.container && -f $before/current.json && -f $before/health.json ]] || { fail 'Previous deployment backup missing'; return 1; }
  grep -Fx "Image=$image" "$before/active.container" >/dev/null || return
  podman image exists "$image" || { fail "Previous image unavailable: $image"; return 1; }
 fi
 if ! systemctl --user stop "$service.service"; then
  if service_is_active; then fail 'Could not stop candidate service'; return 1
  else [[ $? == 1 ]] || return 1; fi
 fi
 restore_release_database "$release" || return
 restore_managed_files "$before" "$restore_id" || return
 check_path "$unit.deploy-install-$restore_id" || return
 rm -f -- "$unit.deploy-install-$restore_id" || return
 if [[ $(jq -r .quadlet "$before/snapshot.json") == true ]]; then
  check_path "$unit.deploy-restore-$restore_id" || return
  cp -p -- "$before/active.container" "$unit.deploy-restore-$restore_id" || return
  durable_move "$unit.deploy-restore-$restore_id" "$unit" || return
 else durable_remove "$unit" || return; fi
 if [[ -f $before/current.json ]]; then
  cat "$before/current.json" | atomic_json "$current" || return
 else durable_remove "$current" "$owner" "$target_owner" || return; fi
 systemctl --user daemon-reload || return
 if [[ $active == true ]]; then
  systemctl --user start "$service.service" || return
  wait_health "$before/health.json" "$image" || { fail 'Previous deployment failed its original health check'; return 1; }
 else
  if service_is_active; then fail 'Previous inactive service is still active'; return 1
  else [[ $? == 1 ]] || return 1; fi
 fi
 durable_remove "$pending" || return
 echo "Restored previous deployment for $service" >&2
}


recover_pending() {
 [[ -f $pending ]] || return 0
 local pending_id release image
 pending_id=$(jq -er .release_id "$pending") || return
 [[ $pending_id =~ ^[a-zA-Z0-9_-]+$ ]] || { fail 'Invalid pending release ID'; return 1; }
 release="$state/releases/$pending_id"
 check_path "$release" || return
 image=$(jq -er .image_id "$release/manifest.json") || return
 if [[ -f $current ]]; then
  validate_commit || return
  if [[ $(jq -r .current "$current") == "$pending_id" ]] && cmp -s "$unit" "$release/candidate.container" && resume_committed_release "$release/manifest.json" "$image"; then
   durable_remove "$pending" || return
   echo "Completed committed pending release $pending_id" >&2
   return 0
  fi
 fi
 if snapshot_unchanged "$release"; then
  discard_prepared "$release" || return
  echo "Discarded unactivated pending release $pending_id" >&2
  return 0
 fi
 echo "Recovering interrupted release $pending_id" >&2
 bounded_recovery restore_image_deployment "$release"
}
if [[ -f $boot_pending ]]; then restore_boot; fi
recover_pending
validate_boot

if [[ $mode == action ]]; then
 action=${3:?missing service action}
 case "$action" in start|stop|restart|enable|disable) ;; *) fail 'Invalid action';; esac
 [[ -f $current ]] || fail 'Service action requires a confirmed image deployment'
 validate_commit
 confirmed_id=$(jq -er .current "$current")
 cmp -s "$unit" "$state/releases/$confirmed_id/candidate.container" || fail 'Active Quadlet differs from confirmed deployment'
 bind_service
 case "$action" in
 enable) set_boot true;;
 disable) set_boot false;;
 *) systemctl --user "$action" "$service.service";;
 esac
 exit 0
fi
[[ $mode == release ]] || fail 'Invalid operation'
[[ ! -e $state/releases/$id ]] || fail 'Release ID already exists'
[[ $(realpath -- "$stage") == "$state/staging/$id" ]] || fail 'Invalid staging directory'
check_path "$stage"
chmod 700 "$stage"
make_dir "$units"
check_path "$unit"

old_id='' old_image='' old_active=false
if service_is_active; then old_active=true; else [[ $? == 1 ]] || exit 1; fi
if [[ -f $current ]]; then
 validate_commit
 old_id=$(jq -er .current "$current")
 old_release="$state/releases/$old_id"
 check_path "$old_release"
 jq -e --arg service "$service" --arg target "$target" '.schema == 1 and .service == $service and .target == $target and .check_result != null' "$old_release/manifest.json" >/dev/null
 old_image=$(canonical_id "$(jq -er .image_id "$old_release/manifest.json")")
 cmp -s "$unit" "$old_release/candidate.container" || fail 'Active Quadlet differs from confirmed deployment'
 grep -Fx "Image=$old_image" "$unit" >/dev/null || fail 'Previous Quadlet is not pinned to its confirmed image'
 grep -Fx 'Pull=never' "$unit" >/dev/null || fail 'Previous Quadlet can pull an unconfirmed image'
 podman image exists "$old_image" || fail 'Confirmed previous image is missing locally'
 if $old_active; then
  actual=$(canonical_id "$(podman container inspect --format '{{.Image}}' "systemd-$service")")
  [[ $actual == "$old_image" ]] || fail 'Running image differs from confirmed deployment'
 fi
else
 [[ ! -e $unit && ! -L $unit ]] || fail 'Existing Quadlet has no confirmed image deployment; use a new service name or explicit migration'
 if $old_active || systemctl --user cat "$service.service" >/dev/null 2>&1; then fail 'Existing unmanaged service cannot be adopted'; fi
fi

check_release_capacity
release="$state/releases/$id"
mkdir -m 700 -- "$release"
before="$release/before"
mkdir -m 700 -- "$before" "$before/files"
jq --arg timestamp "$(date -u +%FT%TZ)" '. + {created_at:$timestamp}' "$request" | atomic_json "$release/manifest.json"
if [[ -n $old_id ]]; then
 cp -p -- "$unit" "$before/active.container"
 cp -p -- "$current" "$before/current.json"
 cp -p -- "$old_release/manifest.json" "$before/health.json"
 old_retention=$(jq -er .retention_tag "$old_release/manifest.json")
 podman tag "$old_image" "$old_retention"
 sync_image_store
fi
jq -n --arg image "$old_image" --argjson active "$old_active" --argjson quadlet "$( [[ -n $old_id ]] && echo true || echo false )" '{image_id:$image,active:$active,quadlet:$quadlet}' | atomic_json "$before/snapshot.json"
printf '[]\n' > "$before/files.json"

# Build a file-by-file installation list. Existing directories are merged, never
# recursively replaced, so unrelated data survives both installation and rollback.

find "$stage/artifacts" -mindepth 1 -print0 | sort -z > "$release/staged-paths"
while IFS= read -r -d '' file; do
 rel=${file#"$stage/artifacts/"}
 [[ $rel != .env ]] || fail 'Environment files must be synchronized separately from artifacts'
 [[ $rel != .deploy && $rel != .deploy/* && $rel != ../* && $rel != /* ]] || fail 'Unsafe staged artifact'
 [[ ! -L $file ]] || fail 'Symlink in staged artifacts'
 if [[ -d $file ]]; then record_path "$rel" directory
 elif [[ -f $file ]]; then record_path "$rel" file
 else fail 'Unsupported staged artifact'; fi
done < "$release/staged-paths"
reconcile_managed_files
if [[ $(jq -r .sync_env "$request") == true ]]; then
 [[ -f $stage/env && ! -L $stage/env ]] || fail 'Missing staged environment file'
 record_path .env file
fi
env_file=$(jq -r '.env_file // ""' "$request")
if [[ -n $env_file ]]; then
 check_path "$env_file"
 if [[ $(jq -r .sync_env "$request") != true ]]; then [[ -f $env_file && -r $env_file ]] || fail 'Referenced environment file does not exist or is unreadable'; fi
fi

reference=$(jq -er .configured_reference "$request")
# --quiet returns the ID of this pull, avoiding a second lookup of a mutable tag.
pulled=$(podman pull --quiet -- "$reference")
image=$(canonical_id "$pulled") || fail 'Pull did not return exactly one valid image ID'
inspection=$(podman image inspect "$image")
[[ $(canonical_id "$(jq -er '.[0].Id' <<< "$inspection")") == "$image" ]] || fail 'Pulled image inspection returned a different ID'
platform_os=$(jq -er '.[0].Os' <<< "$inspection")
platform_arch=$(jq -er '.[0].Architecture' <<< "$inspection")
native_os=$(podman info --format '{{.Host.OS}}')
native_arch=$(podman info --format '{{.Host.Arch}}')
[[ $platform_os == "$native_os" && $platform_arch == "$native_arch" ]] || fail 'Pulled image does not match native target platform'
retention="localhost/deploy-retain/$key:$id"
podman tag "$image" "$retention"
sync_image_store
# Only one exact Image= placeholder line may be substituted.
[[ $(grep -Fxc 'Image=__DEPLOY_RESOLVED_IMAGE__' "$stage/candidate.container") == 1 ]] || fail 'Invalid image placeholder'
sed "s/^Image=__DEPLOY_RESOLVED_IMAGE__$/Image=$image/" "$stage/candidate.container" > "$release/candidate.container"
grep -Fx 'Pull=never' "$release/candidate.container" >/dev/null || fail 'Candidate must disable implicit pulls'
validate_candidate_quadlet
jq --arg image "$image" --arg retention "$retention" --arg previous "$old_id" --arg os "$platform_os" --arg arch "$platform_arch" --arg timestamp "$(date -u +%FT%TZ)" --argjson digests "$(jq '.[0].RepoDigests // []' <<< "$inspection")" '. + {image_id:$image,registry_digests:$digests,retention_tag:$retention,created_at:$timestamp,previous_release:(if $previous == "" then null else $previous end),has_predecessor:($previous != ""),os:$os,architecture:$arch,check_result:null}' "$request" | atomic_json "$release/manifest.json"

[[ ! -e $unit.deploy-install-$id && ! -L $unit.deploy-install-$id ]] || fail 'Reserved Quadlet temporary path already exists'
changed=false
transaction_exit() {
 local status=$?
 trap - EXIT HUP INT TERM
 if (( status != 0 )); then
  if $changed || ! snapshot_unchanged "$release"; then
  echo "Image rollout failed (exit $status); restoring previous deployment" >&2
  if bounded_recovery restore_image_deployment "$release"; then
   echo 'Rollout failed; previous state restored' >&2
  else
   echo "ROLLBACK FAILED after rollout failure (exit $status); pending state and backups preserved at $release" >&2
  fi
  else
   discard_prepared "$release" || echo 'Could not clear unactivated pending state; recovery required' >&2
  fi
 fi
 exit "$status"
}
trap transaction_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
sync -f -- "$release"
bind_service
jq -n --arg id "$id" '{schema:1,release_id:$id}' | atomic_json "$pending"
phase_checkpoint pending
if $old_active && [[ $(jq -r .stop_on_deploy "$request") == true ]]; then systemctl --user stop "$service.service"; changed=true; fi
check_release_capacity
prepare_release_database
install_managed_files "$before" "$stage/artifacts"
check_path "$unit.deploy-install-$id"
cp -- "$release/candidate.container" "$unit.deploy-install-$id"
chmod 600 "$unit.deploy-install-$id"
durable_move "$unit.deploy-install-$id" "$unit"
phase_checkpoint candidate-activation
changed=true
systemctl --user daemon-reload
if [[ -f $before/database.json ]]; then printf '{}\n' | atomic_json "$release/database-started.json"; fi
systemctl --user restart "$service.service"
phase_checkpoint candidate-started
wait_health "$release/manifest.json" "$image" || fail 'Readiness deadline exceeded (service, image identity or application health check)'
check_result=application-health
if [[ $(jq -r .health_command "$request") != true && -z $(jq -r .health_url "$request") ]]; then check_result=service-and-image-only; fi
jq --arg result "$check_result" --arg timestamp "$(date -u +%FT%TZ)" '.check_result=$result | .confirmed_at=$timestamp' "$release/manifest.json" | atomic_json "$release/manifest.json"
jq -n --arg id "$id" --arg previous "$old_id" --arg service "$service" --arg target "$target" '{schema:1,current:$id,previous:(if $previous == "" then null else $previous end),service:$service,target:$target}' | atomic_json "$current"
# Once committed, a disconnect is resolved by checking current.json and readiness.
phase_checkpoint commit
changed=false
durable_remove "$pending"
phase_checkpoint pending-cleared
printf 'Confirmed release %s; commit %s; image %s; readiness: %s\n' "$id" "$(jq -r .git_commit "$request")" "$image" "$check_result"
