# guard_operation already holds the service and target locks and rejects pending
# recovery. Fail closed on any corrupt manifest before applying a deletion list.
store_lock -x
mode=$(jq -er .mode <<< "$configuration")
case "$mode" in image|binary) ;; *) fail 'Invalid retention mode';; esac
for record in "$owner" "$target_owner"; do
 if [[ -f $record ]]; then jq -e --arg mode "$mode" '(.mode // "image") == $mode' "$record" >/dev/null || fail 'Cleanup deployment mode differs from the service owner'; fi
done
state="$target/.deploy/$mode"
check_path "$state"
workspace=$(mktemp -d)
trap 'rm -rf -- "$workspace"' EXIT
printf '[]\n' > "$workspace/releases.json"
printf '[]\n' > "$workspace/backups.json"
printf '[]\n' > "$workspace/operations.json"
printf '[]\n' > "$workspace/protected-backups.json"
protected='[]'
if [[ -f $state/current.json ]]; then
 jq -e --arg service "$service" --arg target "$target" '.schema == 1 and .service == $service and .target == $target and (.current | test("^[a-zA-Z0-9_-]+$")) and (.previous == null or (.previous | test("^[a-zA-Z0-9_-]+$")))' "$state/current.json" >/dev/null
 protected=$(jq '[.current,.previous] | map(select(. != null))' "$state/current.json")
fi
if [[ -d $state/releases ]]; then
 find "$state/releases" -mindepth 1 -maxdepth 1 -type d -print0 > "$workspace/paths"
 while IFS= read -r -d '' directory; do
  release_id=${directory##*/}
  [[ $release_id =~ ^[a-zA-Z0-9_-]+$ ]] || fail 'Unsafe release inventory'
  check_path "$directory/manifest.json"
  jq -e --arg service "$service" --arg target "$target" --arg id "$release_id" '.schema == 1 and .service == $service and .target == $target and .release_id == $id' "$directory/manifest.json" >/dev/null || fail "Incomplete or corrupt release inventory: $directory"
  mtime=$(stat -c %Y -- "$directory")
  jq --arg id "$release_id" --arg path "$directory" --argjson mtime "$mtime" '. + [{id:$id,path:$path,mtime:$mtime}]' "$workspace/releases.json" > "$workspace/next"
  mv -- "$workspace/next" "$workspace/releases.json"
 done < "$workspace/paths"
fi
while IFS= read -r release_id; do
 jq -e --arg id "$release_id" 'any(.[]; .id == $id)' "$workspace/releases.json" >/dev/null || fail 'Referenced release is missing; cleanup refused'
done < <(jq -r '.[]' <<< "$protected")
operations="$target/.deploy/db/operations"
check_path "$operations"
if [[ -d $operations ]]; then
 find "$operations" -mindepth 1 -maxdepth 1 -type d -print0 > "$workspace/paths"
 while IFS= read -r -d '' directory; do
  operation_id=${directory##*/}
  [[ $operation_id =~ ^[a-zA-Z0-9_-]+$ ]] || fail 'Unsafe database operation inventory'
  check_path "$directory/manifest.json"
  # Earlier releases did not inventory completed operations; keep those copies.
  [[ -f $directory/manifest.json ]] || continue
  jq -e --arg service "$service" --arg target "$target" --arg id "$operation_id" '.schema == 1 and .service == $service and .target == $target and .release_id == $id' "$directory/manifest.json" >/dev/null || fail 'Corrupt database operation inventory'
  mtime=$(stat -c %Y -- "$directory")
  jq --arg id "$operation_id" --arg path "$directory" --argjson mtime "$mtime" '. + [{id:$id,path:$path,mtime:$mtime}]' "$workspace/operations.json" > "$workspace/next"
  mv -- "$workspace/next" "$workspace/operations.json"
 done < "$workspace/paths"
fi
now=$(date +%s)
jq --argjson protected "$protected" --argjson policy "$(jq .releases <<< "$configuration")" --argjson now "$now" '
 sort_by(.mtime,.id) | reverse | to_entries | map(.value as $entry | $entry + {delete:($policy.enabled and (.key >= $policy.keep) and ($policy.age_seconds == 0 or $entry.mtime < ($now - $policy.age_seconds)) and (($protected | index($entry.id)) == null))})
' "$workspace/releases.json" > "$workspace/release-plan.json"
jq --argjson policy "$(jq .releases <<< "$configuration")" --argjson now "$now" '
 sort_by(.mtime,.id) | reverse | to_entries | map(.value as $entry | $entry + {delete:($policy.enabled and (.key >= $policy.keep) and ($policy.age_seconds == 0 or $entry.mtime < ($now - $policy.age_seconds)))})
' "$workspace/operations.json" > "$workspace/operation-plan.json"

# Retained releases can hold database recovery paths in more than one marker.
jq -r '.[] | select(.delete == false) | .path' "$workspace/release-plan.json" "$workspace/operation-plan.json" > "$workspace/kept-paths"
while IFS= read -r directory; do
 find "$directory" -type f -name '*.json' -print0 > "$workspace/metadata-paths"
 while IFS= read -r -d '' metadata; do
  check_path "$metadata"
  jq -s '.[0] + [.[1:] | .[] | .. | strings | select(startswith("/"))]' "$workspace/protected-backups.json" "$metadata" > "$workspace/next"
  mv -- "$workspace/next" "$workspace/protected-backups.json"
 done < "$workspace/metadata-paths"
done < "$workspace/kept-paths"
database=$(jq -r .database <<< "$configuration")
environment=$(jq -r .environment <<< "$configuration")
if [[ -n $database ]]; then
 uid=$(jq -r '.container_uid // 0' <<< "$configuration")
 backup_dir="$(dirname -- "$database")/backup"
 db_check_path "$uid" "$backup_dir"
 if db_fileop "$uid" test -d "$backup_dir"; then
  db_fileop "$uid" find "$backup_dir" -maxdepth 1 -type f -name '*.meta.json' -print0 > "$workspace/metadata-paths"
  while IFS= read -r -d '' metadata; do
   db_check_path "$uid" "$metadata"
   db_fileop "$uid" jq -e '.schema == 1 and (.snapshot | type == "string") and (.sha256 | test("^[0-9a-f]{64}$"))' "$metadata" >/dev/null || fail "Corrupt backup inventory: $metadata"
   if ! db_fileop "$uid" jq -e --arg database "$database" --arg env "$environment" '.database == $database and .environment == $env' "$metadata" >/dev/null; then continue; fi
   name=$(db_fileop "$uid" jq -er .snapshot "$metadata")
   [[ $name != */* && $name != .* && $name == *.db && $metadata == "$backup_dir/$name.meta.json" ]] || fail 'Unsafe backup inventory path'
   file="$backup_dir/$name"
   db_check_path "$uid" "$file"
   db_fileop "$uid" test -f "$file" || fail 'Backup inventory references a missing snapshot'
   mtime=$(db_fileop "$uid" stat -c %Y -- "$file")
   jq --arg path "$file" --arg metadata "$metadata" --argjson mtime "$mtime" '. + [{path:$path,metadata:$metadata,mtime:$mtime}]' "$workspace/backups.json" > "$workspace/next"
   mv -- "$workspace/next" "$workspace/backups.json"
  done < "$workspace/metadata-paths"
 fi
fi
jq --slurpfile protected "$workspace/protected-backups.json" --argjson policy "$(jq .backups <<< "$configuration")" --argjson now "$now" '
 sort_by(.mtime,.path) | reverse | to_entries | map(.value as $entry | $entry + {delete:($policy.enabled and (.key >= $policy.keep) and ($policy.age_seconds == 0 or $entry.mtime < ($now - $policy.age_seconds)) and (($protected[0] | index($entry.path)) == null))})
' "$workspace/backups.json" > "$workspace/backup-plan.json"
preview=$(jq -r .preview <<< "$configuration")
jq -r '.[] | (if .delete then "DELETE " else "KEEP protected or within retention " end) + .path' "$workspace/release-plan.json" "$workspace/operation-plan.json" "$workspace/backup-plan.json"
jq -r '.[] | select(.delete) | .path' "$workspace/backup-plan.json" > "$workspace/deleted-backups"
# Validate all selected snapshot contents before deleting any history directory.
while IFS= read -r file; do
 db_check_path "$uid" "$file"
 checksum=$(db_fileop "$uid" sha256sum -- "$file" | cut -d' ' -f1)
 [[ $checksum == "$(db_fileop "$uid" jq -er .sha256 "$file.meta.json")" ]] || fail 'Backup checksum mismatch; cleanup refused'
done < "$workspace/deleted-backups"
[[ $preview != true ]] || exit 0
while IFS= read -r directory; do
 release_id=${directory##*/}
 # Remove only this release's named references. Other projects retain their own
 # tags even when an image ID is shared. Never remove an image by ID here.
 for tag in "localhost/deploy-retain/$key:$release_id" "localhost/deploy-retain/$key:before-$release_id"; do
  if podman image exists "$tag"; then
   image=$(canonical_id "$(podman image inspect "$tag" | jq -er '.[0].Id')")
   podman untag "$image" "$tag"
  fi
 done
 check_path "$directory"
 [[ $directory == "$state/releases/$release_id" ]] || fail 'Deletion escaped release inventory'
 rm -rf -- "$directory"
 sync -f -- "$state/releases"
done < <(jq -r '.[] | select(.delete) | .path' "$workspace/release-plan.json")
while IFS= read -r directory; do
 check_path "$directory"
 operation_id=${directory##*/}
 [[ $directory == "$operations/$operation_id" && $operation_id =~ ^[a-zA-Z0-9_-]+$ ]] || fail 'Deletion escaped database operation inventory'
 rm -rf -- "$directory"
 sync -f -- "$operations"
done < <(jq -r '.[] | select(.delete) | .path' "$workspace/operation-plan.json")
while IFS= read -r file; do
 db_check_path "$uid" "$file"
 checksum=$(db_fileop "$uid" sha256sum -- "$file" | cut -d' ' -f1)
 [[ $checksum == "$(db_fileop "$uid" jq -er .sha256 "$file.meta.json")" ]] || fail 'Backup changed after inventory; cleanup refused'
 db_fileop "$uid" rm -f -- "$file" "$file.meta.json"
 db_fileop "$uid" sync -f -- "$backup_dir"
done < "$workspace/deleted-backups"
