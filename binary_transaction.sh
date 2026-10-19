mode=${1:?missing operation}
stage=${2:?missing staging path}
init_transaction binary "$stage"
recover_database_update
init_boot_state

restore_binary_image_reference() {
 local before="$1/before" old_image old_ref
 old_image=$(jq -r .image_id "$before/snapshot.json") || return
 old_ref=$(jq -r .image_reference "$before/snapshot.json") || return
 # Digest references and image IDs are immutable and cannot be retagged.
 if [[ -n $old_image && $old_ref != *@* && $old_ref != "$old_image" && $old_ref != "${old_image#sha256:}" ]]; then
  podman tag "$old_image" "$old_ref" || return
  sync_image_store || return
 fi
}

discard_binary_prepared() {
 # A build can move a legacy tag even when all managed files are unchanged.
 restore_binary_image_reference "$1" || return
 discard_prepared "$1"
}

restore_binary_deployment() {
 local release=$1 before="$1/before" restore_id=${1##*/} old_image active
 [[ -f $before/snapshot.json ]] || return 1
 validate_managed_snapshot "$before" || return
 old_image=$(jq -r .image_id "$before/snapshot.json") || return
 active=$(jq -r .active "$before/snapshot.json") || return
 if [[ -n $old_image ]]; then podman image exists "$old_image" || return; fi
 if ! systemctl --user stop "$service.service"; then
  if service_is_active; then return 1; else [[ $? == 1 ]] || return 1; fi
 fi
 restore_release_database "$release" || return
 restore_managed_files "$before" "$restore_id" || return
 if [[ $(jq -r .quadlet "$before/snapshot.json") == true ]]; then
  check_path "$unit.deploy-restore-$restore_id" || return
  cp -p -- "$before/active.container" "$unit.deploy-restore-$restore_id" || return
  durable_move "$unit.deploy-restore-$restore_id" "$unit" || return
  restore_binary_image_reference "$release" || return
 else durable_remove "$unit" || return; fi
 rm -f -- "$unit.deploy-install-$restore_id" || return
 if [[ -f $before/current.json ]]; then
  atomic_json "$current" < "$before/current.json" || return
 else durable_remove "$current" "$owner" "$target_owner" || return; fi
 systemctl --user daemon-reload || return
 if [[ $active == true ]]; then
  systemctl --user start "$service.service" || return
  wait_health "$before/health.json" "$old_image" || return
 fi
 durable_remove "$pending" || return
 echo "Restored previous binary deployment for $service" >&2
}

if [[ -f $boot_pending ]]; then restore_boot; fi
if [[ -f $pending ]]; then
 pending_id=$(jq -er .release_id "$pending")
 release="$state/releases/$pending_id"
 image=$(jq -r '.image_id // ""' "$release/manifest.json")
 if [[ -f $current && $(jq -r .current "$current") == "$pending_id" && -n $image ]] && cmp -s "$unit" "$release/candidate.container" && wait_health "$release/manifest.json" "$image"; then
  durable_remove "$pending"
 elif snapshot_unchanged "$release"; then discard_binary_prepared "$release"
 else restore_binary_deployment "$release"; fi
fi
validate_boot

if [[ $mode == action ]]; then
 action=${3:?missing service action}
 case "$action" in start|stop|restart|enable|disable) ;; *) fail 'Invalid action';; esac
 if [[ -f $current ]]; then
  confirmed=$(jq -er .current "$current")
  cmp -s "$unit" "$state/releases/$confirmed/candidate.container" || fail 'Active Quadlet differs from confirmed deployment'
 else [[ -f $unit ]] || fail 'Service action requires an existing binary deployment'; fi
 bind_service
 case "$action" in enable) set_boot true;; disable) set_boot false;; *) systemctl --user "$action" "$service.service";; esac
 exit 0
fi
[[ $mode == release && $(realpath -- "$stage") == "$state/staging/$id" ]] || fail 'Invalid binary release staging directory'
[[ ! -e $state/releases/$id ]] || fail 'Release ID already exists'
make_dir "$units"
check_path "$unit"
release="$state/releases/$id"
before="$release/before"
mkdir -m 700 -- "$release" "$before" "$before/files"
old_active=false old_image='' old_ref='' old_id='' old_unit=false
if service_is_active; then old_active=true; else [[ $? == 1 ]] || exit 1; fi
if [[ -f $current ]]; then
 old_id=$(jq -er .current "$current")
 [[ $old_id =~ ^[a-zA-Z0-9_-]+$ ]] || fail 'Invalid previous release ID'
 cmp -s "$unit" "$state/releases/$old_id/candidate.container" || fail 'Active Quadlet differs from confirmed deployment'
 cp -p -- "$current" "$before/current.json"
 cp -- "$state/releases/$old_id/manifest.json" "$before/health.json"
fi
if [[ -f $unit ]]; then
 old_unit=true
 cp -p -- "$unit" "$before/active.container"
 [[ $(grep -c '^Image=' "$unit") == 1 ]] || fail 'Previous Quadlet has no unique image reference'
 old_ref=$(sed -n 's/^Image=//p' "$unit")
 if $old_active; then
  old_image=$(canonical_id "$(podman container inspect --format '{{.Image}}' "systemd-$service")")
 else old_image=$(canonical_id "$(podman image inspect "$old_ref" | jq -er '.[0].Id')"); fi
 podman image exists "$old_image" || fail 'Previous image is unavailable'
 podman tag "$old_image" "localhost/deploy-retain/$key:before-$id"
 sync_image_store
 if [[ ! -f $before/health.json ]]; then
  old_command=false
  if grep -q '^HealthCmd=' "$unit"; then old_command=true; fi
  jq -n --argjson command "$old_command" '{health_seconds:30,health_command:$command,health_url:""}' | atomic_json "$before/health.json"
 fi
elif $old_active || systemctl --user cat "$service.service" >/dev/null 2>&1; then
 fail 'Existing service has no restorable Quadlet'
fi
jq -n --arg image "$old_image" --arg reference "$old_ref" --argjson active "$old_active" --argjson quadlet "$old_unit" '{image_id:$image,image_reference:$reference,active:$active,quadlet:$quadlet}' | atomic_json "$before/snapshot.json"
printf '[]\n' > "$before/files.json"
find "$stage/artifacts" -mindepth 1 -print0 | sort -z > "$release/staged-paths"
while IFS= read -r -d '' file; do
 rel=${file#"$stage/artifacts/"}
 [[ $rel != .env ]] || fail 'Environment files must be synchronized separately from build artifacts'
 [[ $target/$rel != "$(jq -r '.env_file // ""' "$request")" ]] || fail 'Configured environment file cannot be a build artifact'
 [[ ! -L $file ]] || fail 'Symlinks are not supported in transactional binary artifacts'
 if [[ -d $file ]]; then record_path "$rel" directory
 elif [[ -f $file ]]; then record_path "$rel" file
 else fail 'Unsupported binary artifact'; fi
done < "$release/staged-paths"
dockerfile=$(jq -er .dockerfile "$request")
[[ $dockerfile != /* && /$dockerfile/ != */../* && -f $stage/artifacts/$dockerfile && ! -L $stage/artifacts/$dockerfile ]] || fail 'Dockerfile is missing from staged build artifacts'
reconcile_managed_files
if [[ $(jq -r .sync_env "$request") == true ]]; then
 [[ -f $stage/env && ! -L $stage/env ]] || fail 'Missing staged environment file'
 record_path .env file
fi
env_file=$(jq -r '.env_file // ""' "$request")
if [[ -n $env_file && $(jq -r .sync_env "$request") != true ]]; then
 check_path "$env_file"
 [[ -r $env_file && -f $env_file ]] || fail 'Referenced environment file is missing or unreadable'
fi
cp -- "$request" "$release/manifest.json"
sync -f -- "$release"
changed=false
transaction_exit() {
 local status=$?
 trap - EXIT HUP INT TERM
 if (( status != 0 )); then
  if $changed || ! snapshot_unchanged "$release"; then
   if restore_binary_deployment "$release"; then echo 'Binary rollout failed; previous state restored' >&2
   else echo "ROLLBACK FAILED; pending state and backups preserved at $release" >&2; fi
  else discard_binary_prepared "$release" || echo 'Pending recovery required' >&2; fi
 fi
 exit "$status"
}
trap transaction_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
bind_service
jq -n --arg id "$id" '{schema:1,release_id:$id}' | atomic_json "$pending"
if $old_active && [[ $(jq -r .stop_on_deploy "$request") == true ]]; then systemctl --user stop "$service.service"; changed=true; fi
prepare_release_database
install_managed_files "$before" "$stage/artifacts"
reference=$(jq -er .configured_reference "$request")
dockerfile=$(jq -er .dockerfile "$request")
cd -- "$stage/artifacts"
podman build --iidfile "$release/built-image-id" -f "$dockerfile" -t "$reference" .
# Capture this build's ID directly: another service may reuse the mutable tag.
image=$(canonical_id "$(cat -- "$release/built-image-id")")
podman tag "$image" "localhost/deploy-retain/$key:$id"
sync_image_store
uid=$(jq -r .container_uid "$request")
gid=$(jq -r .container_gid "$request")
if (( uid > 0 )); then
 jq -r '(.chown_paths // [])[]' "$request" > "$release/chown-paths"
 database=$(jq -r '.database // ""' "$request")
 backup_directory=''
 if [[ -n $database ]]; then backup_directory="$(dirname -- "$database")/backup"; fi
 while IFS= read -r owned; do
  if [[ -n $backup_directory ]]; then
   # Keep historical backups owned by the SSH user, outside application ownership.
   podman unshare find "$owned" -path "$backup_directory" -prune -o -exec chown --no-dereference "$uid:$gid" -- '{}' +
  else podman unshare chown -R "$uid:$gid" "$owned"; fi
 done < "$release/chown-paths"
fi
[[ $(grep -c '^Image=' "$stage/candidate.container") == 1 ]] || fail 'Candidate has no unique image reference'
sed "s/^Image=.*$/Image=$image\nPull=never/" "$stage/candidate.container" > "$release/candidate.container"
check_path "$unit.deploy-install-$id"
cp -- "$release/candidate.container" "$unit.deploy-install-$id"
chmod 600 "$unit.deploy-install-$id"
durable_move "$unit.deploy-install-$id" "$unit"
changed=true
systemctl --user daemon-reload
if [[ -f $before/database.json ]]; then printf '{}\n' | atomic_json "$release/database-started.json"; fi
systemctl --user restart "$service.service"
wait_health "$release/manifest.json" "$image" || fail 'Binary readiness deadline exceeded'
jq --arg image "$image" '.image_id=$image | .check_result="confirmed"' "$release/manifest.json" | atomic_json "$release/manifest.json"
jq -n --arg id "$id" --arg previous "$old_id" --arg service "$service" --arg target "$target" '{schema:1,current:$id,previous:(if $previous == "" then null else $previous end),service:$service,target:$target}' | atomic_json "$current"
changed=false
durable_remove "$pending"
echo 'Binary deployment confirmed'
