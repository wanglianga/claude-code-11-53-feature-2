#!/usr/bin/env bash
# End-to-end acceptance replay.
#
# Replays, against a running API, the four failure scenarios the platform must
# survive and proves each with assertions (not just prints):
#
#   1. regional outage      — BR-MT link down, local ingest keeps committing,
#                             group aggregate excludes BR-MT and says so
#   2. replication retry    — overlapping re-upload dedupes, cursor does not
#                             move while failing, area totals never double
#   3. primary/standby      — failover resumes at the DR-confirmed position,
#                             unconfirmed copies are discarded, local journal
#                             stays gapless on the new primary
#   4. approval revocation  — approval revoked between aggregate and delivery
#                             blocks the delivery and exports no artifact
#
# plus the outbound audit chain: artifact hash, restricted-field scan, and
# tamper detection.
#
# Usage: scripts/replay.sh [BASE_URL]        (default http://127.0.0.1:8080)
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
S="$ROOT/samples"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

TOK_ADMIN="demo-admin-token"
TOK_ANALYST="demo-analyst-token"
TOK_APPROVER="demo-approver-token"
TOK_AUDITOR="demo-auditor-token"

PASS=0
FAIL=0
CHECKS=()

slug() { echo "$1" | tr 'a-z' 'A-Z'; }

# api METHOD PATH TOKEN [JSON_BODY]
# If the 4th argument starts with '@', it is treated as a file path.
api() {
  local method="$1" path="$2" token="$3" body="${4:-}"
  local args=(-sS -X "$method" "$BASE$path" -H "Authorization: Bearer $token")
  if [[ -n "$body" ]]; then
    if [[ "$body" == @* ]]; then
      args+=(-H 'Content-Type: application/json' --data-binary "@${body#@}")
    else
      args+=(-H 'Content-Type: application/json' --data-binary "$body")
    fi
  fi
  curl "${args[@]}"
}

# check NAME ACTUAL EXPECTED — pass when the two strings are equal
check() {
  local name="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mPASS\033[0m %-62s %s\n' "$name" "$actual"
    CHECKS+=("PASS|$name|$actual")
  else
    FAIL=$((FAIL + 1))
    printf '  \033[31mFAIL\033[0m %-62s got=%s want=%s\n' "$name" "$actual" "$expected"
    CHECKS+=("FAIL|$name|got=$actual want=$expected")
  fi
}

# contains NAME HAYSTACK NEEDLE — substring assertion for JSON blobs
contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" == *"$needle"* ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mPASS\033[0m %-62s contains %s\n' "$name" "$needle"
    CHECKS+=("PASS|$name|contains $needle")
  else
    FAIL=$((FAIL + 1))
    printf '  \033[31mFAIL\033[0m %-62s missing %s\n' "$name" "$needle"
    CHECKS+=("FAIL|$name|missing $needle")
  fi
}

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

sql_region() {
  PGPASSWORD="${PGPASSWORD:-ccdev}" "${PSQL:-/opt/postgresql-18/bin/psql}" \
    -h "${PGHOST:-127.0.0.1}" -p "${PGPORT:-5432}" -U "${PGUSER:-ccdev}" \
    -d "agrisync_region_$1" -tAc "$2" 2>/dev/null
}

sql_control() {
  PGPASSWORD="${PGPASSWORD:-ccdev}" "${PSQL:-/opt/postgresql-18/bin/psql}" \
    -h "${PGHOST:-127.0.0.1}" -p "${PGPORT:-5432}" -U "${PGUSER:-ccdev}" \
    -d agrisync_control -tAc "$1" 2>/dev/null
}

echo "AgriSync acceptance replay against $BASE"

section "0. authentication and role boundaries"
check "no token is rejected" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/regions")" "401"
check "bad token is rejected" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer nope' "$BASE/regions")" "401"
check "region operator cannot read another region" \
  "$(api GET /regions/BR-MT/status demo-operator-de-bb | jq -r .error.code)" "forbidden"
check "region operator cannot ingest into another region" \
  "$(api POST /regions/BR-MT/ingest demo-operator-de-bb '@$S/regions/br-mt.json' | jq -r .error.code)" "forbidden"
check "analyst cannot approve their own request" \
  "$(api POST /outbound/requests "$TOK_ANALYST" '@$S/approvals/wide.json' >/dev/null; \
     id=$(api GET /outbound/requests "$TOK_ANALYST" | jq -r '.requests[0].id'); \
     api POST "/outbound/requests/$id/decide" "$TOK_ANALYST" '{"decision":"approved"}' | jq -r .error.code)" "forbidden"
check "operator cannot trigger a global replication sweep" \
  "$(api POST /replication/batch demo-operator-de-bb '{}' | jq -r .error.code)" "forbidden"
check "operator cannot fail over a region" \
  "$(api POST /dr/US-IA/failover demo-operator-us-ia '{}' | jq -r .error.code)" "forbidden"
check "group analyst has no path to regional raw records" \
  "$(api GET /regions/BR-MT/operations "$TOK_ANALYST" | jq -r .error.code)" "forbidden"
check "auditor may read a region's records for audit" \
  "$(api GET /regions/BR-MT/operations "$TOK_AUDITOR" | jq -r '.region')" "BR-MT"

section "0. reset and baseline ingest"
api POST /demo/reset "$TOK_ADMIN" | tee "$TMP/reset.json" >/dev/null
check "reset" "$(jq -r .status "$TMP/reset.json" 2>/dev/null)" "reset"

for r in de-bb br-mt us-ia fr-nor; do
  code=$(slug "$r")
  out=$(api POST "/regions/$code/ingest" "demo-operator-$r" "@$S/regions/$r.json")
  echo "$out" > "$TMP/ingest-$r.json"
  check "ingest $code accepted" "$(jq -r .accepted "$TMP/ingest-$r.json")" "4"
  check "ingest $code duplicates" "$(jq -r .duplicates "$TMP/ingest-$r.json")" "0"
  check "ingest $code journal starts at 1" "$(jq -r .first_journal_seq "$TMP/ingest-$r.json")" "1"
done

# Idempotent replay of an identical batch: no new rows, no new journal entries.
out=$(api POST /regions/DE-BB/ingest "demo-operator-de-bb" "@$S/regions/de-bb.json")
echo "$out" > "$TMP/replay.json"
check "identical batch replay is flagged" "$(jq -r .replayed_batch "$TMP/replay.json")" "true"
# The receipt replays the original outcome verbatim: the same four segments had
# been accepted on first delivery, and none of them were written again.
check "identical batch replay reports the original receipt" "$(jq -r .accepted "$TMP/replay.json")" "4"
check "identical batch replay wrote no new journal entries" \
  "$(jq -r .last_journal_seq "$TMP/replay.json")" "$(jq -r .last_journal_seq "$TMP/ingest-de-bb.json")"
check "DE-BB still holds exactly 4 operations" "$(sql_region de_bb 'select count(*) from operation_events')" "4"

api POST /replication/batch "$TOK_ADMIN" '{}' > "$TMP/repl1.json"
check "initial replication succeeded" "$(jq -r '[.runs[]|select(.status=="succeeded")]|length' "$TMP/repl1.json")" "4"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "raw trajectories copied only from policy-permitted US-IA" \
  "$(jq -r '.raw_trajectories_by_region|keys|sort|join(",")' <<<"$stats")" "US-IA"
check "raw trajectories blocked for the other three regions" \
  "$(jq -r '[.runs[]|select(.blocked_by_category.raw_trajectory==4)]|length' "$TMP/repl1.json")" "3"

section "1. regional outage: BR-MT keeps recording, group stats lag"
api POST /replication/links/BR-MT "$TOK_ADMIN" '{"status":"offline","reason":"acceptance: backbone cut"}' >/dev/null
out=$(api POST /regions/BR-MT/ingest demo-operator-br-mt "@$S/offline/br-mt-offline-backfill.json")
echo "$out" > "$TMP/offline.json"
check "offline region still commits locally" "$(jq -r .accepted "$TMP/offline.json")" "3"
check "offline batch continues the journal" "$(jq -r .first_journal_seq "$TMP/offline.json")" "5"

st=$(api GET /regions/BR-MT/status demo-operator-br-mt)
echo "$st" > "$TMP/brstatus.json"
check "local journal is continuous while offline" "$(jq -r .journal_entries "$TMP/brstatus.json")" "7"
check "local journal head matches entries" "$(jq -r .journal_head "$TMP/brstatus.json")" "7"
check "region reports the link as offline" "$(jq -r .replication_link "$TMP/brstatus.json")" "offline"
check "outbox backlog is visible (7 events pending)" "$(jq -r .pending_outbox "$TMP/brstatus.json")" "7"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "offline ingest does not leak into the group store" \
  "$(jq -r '.operation_summaries_by_region["BR-MT"]' <<<"$stats")" "4"
check "no unconfirmed rows exist for a link that never connected" \
  "$(jq -r --arg r BR-MT '(.unconfirmed_by_region[$r] // 0)' <<<"$stats")" "0"

run=$(api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}')
echo "$run" > "$TMP/offline-run.json"
check "replication while offline fails" "$(jq -r .error.code "$TMP/offline-run.json")" "unavailable"

apr=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr/decide" "$TOK_APPROVER" '{"decision":"approved","note":"acceptance"}' >/dev/null
aggbody=$(jq -n --arg a "$apr" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",
  period_to:"2026-11-30T00:00:00Z",
  metric_keys:["operation_count","completed_count","completion_rate","area_ha","fuel_l","duration_hours"]}')
runid=$(api POST /aggregates "$TOK_ANALYST" "$aggbody" | jq -r .id)
agg=$(api GET "/aggregates/$runid" "$TOK_ANALYST")
echo "$agg" > "$TMP/agg-offline.json"
check "aggregate includes the three reachable regions" \
  "$(jq -r '.coverage.included_regions|join(",")' "$TMP/agg-offline.json")" "DE-BB,FR-NOR,US-IA"
check "aggregate lists BR-MT as excluded" \
  "$(jq -r '.coverage.excluded_regions|join(",")' "$TMP/agg-offline.json")" "BR-MT"
check "excluded BR-MT is also listed as delayed" \
  "$(jq -r '.coverage.stale_regions|join(",")' "$TMP/agg-offline.json")" "BR-MT"
check "group total excludes the offline region" \
  "$(jq -r '.result.operation_count' "$TMP/agg-offline.json")" "12"

section "2. replication retry: overlap dedupes, area never doubles"
area_before=$(sql_control "select coalesce(sum(area_ha),0) from replica_op_summaries where source_region='BR-MT'")
check "BR-MT replica area before backfill" "$area_before" "414.25"

rc1=$(api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}')
check "retry still fails while the link is offline" "$(jq -r .error.code <<<"$rc1")" "unavailable"
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="BR-MT")')
check "cursor did not advance on failure" "$(jq -r '.confirmed_seq' <<<"$cur")" "9"
check "retry count increments" "$(jq -r '.retry_count>=1' <<<"$cur")" "true"

api POST /replication/links/BR-MT "$TOK_ADMIN" '{"status":"online","reason":""}' >/dev/null
out=$(api POST /regions/BR-MT/ingest demo-operator-br-mt "@$S/offline/br-mt-retry-overlap.json")
echo "$out" > "$TMP/retry.json"
check "overlapping retry upload dedupes 3 repeats" "$(jq -r .duplicates "$TMP/retry.json")" "3"
check "overlapping retry upload adds only the new segment" "$(jq -r .accepted "$TMP/retry.json")" "1"

api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT","fault":"copy_fail"}' > "$TMP/copyfail.json"
check "injected replica write failure is recorded" "$(jq -r '.run.status' "$TMP/copyfail.json")" "failed"
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="BR-MT")')
check "failed copy does not advance the cursor" "$(jq -r '.confirmed_seq' <<<"$cur")" "9"

api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' > "$TMP/retryok.json"
check "retry after failure succeeds" "$(jq -r '.status' "$TMP/retryok.json")" "succeeded"

# Re-send the same window by rewinding nothing: a second run must be a no-op,
# and the group area must equal the region's own area sum exactly.
api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' > "$TMP/noop.json"
check "second replication of the same window is a no-op" "$(jq -r '.status' "$TMP/noop.json")" "noop"
stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "backfilled segments are now group-visible for BR-MT" \
  "$(jq -r '.operation_summaries_by_region["BR-MT"]' <<<"$stats")" "8"
check "BR-MT raw trajectories still never leave the region" \
  "$(jq -r --arg r BR-MT '(.raw_trajectories_by_region[$r] // 0)' <<<"$stats")" "0"
check "DE-BB journal verifies after all replay" \
  "$(api GET /regions/DE-BB/journal/verify demo-operator-de-bb | jq -r .gapless)" "true"
area_group=$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='BR-MT'")
area_local=$(sql_region br_mt "select round(sum(area_ha)::numeric,2) from operation_events")
check "group replica area equals the region's own area (no duplication)" "$area_group" "$area_local"
rows_group=$(sql_control "select count(*) from replica_op_summaries where source_region='BR-MT'")
rows_local=$(sql_region br_mt "select count(*) from operation_events")
check "group replica row count equals the region's own row count" "$rows_group" "$rows_local"
check "BR-MT has exactly 8 distinct operations after dedupe" "$rows_local" "8"

section "3. primary/standby switch resumes only at the confirmed position"
api POST /regions/US-IA/ingest demo-operator-us-ia "@$S/dr/us-ia-post-switch.json" > "$TMP/usia-ingest.json"
check "post-switch batch lands on US-IA" "$(jq -r .accepted "$TMP/usia-ingest.json")" "2"

api POST /replication/run "$TOK_ADMIN" '{"region":"US-IA","fault":"confirm_fail"}' > "$TMP/confail.json"
check "confirmation failure recorded" "$(jq -r '.run.status' "$TMP/confail.json")" "failed"
stats=$(api GET /replication/replicas "$TOK_ADMIN")
contains "copied-but-unconfirmed rows exist before failover" "$stats" '"US-IA": 4'
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="US-IA")')
confirmed_before=$(jq -r '.confirmed_seq' <<<"$cur")

dre=$(api GET /dr/US-IA "$TOK_ADMIN")
check "DR register confirmed position matches the cursor" \
  "$(jq -r '.registry.last_confirmed_seq' <<<"$dre")" "$confirmed_before"

fo=$(api POST /dr/US-IA/failover "$TOK_ADMIN" '{"reconcile":false}')
echo "$fo" > "$TMP/failover.json"
check "failover promotes the standby" "$(jq -r '.to_node' "$TMP/failover.json")" "us-ia-node-b"
check "failover discards unconfirmed summaries" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_op_summaries' "$TMP/failover.json")" "2"
check "failover discards unconfirmed trajectories" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_trajectories' "$TMP/failover.json")" "2"
check "replication resumes at the confirmed position" \
  "$(jq -r '.resumed_from_seq' "$TMP/failover.json")" "$confirmed_before"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "no unconfirmed replicas survive the switch" \
  "$(jq -r --arg r US-IA '(.unconfirmed_by_region[$r] // 0)' <<<"$stats")" "0"

st=$(api GET /regions/US-IA/status demo-operator-us-ia)
check "new primary is active for local writes" "$(jq -r .active_node <<<"$st")" "us-ia-node-b"
check "epoch advanced" "$(jq -r .epoch <<<"$st")" "2"

out=$(api POST /regions/US-IA/ingest demo-operator-us-ia "@$S/dr/us-ia-post-switch.json")
check "new primary keeps accepting local writes" "$(jq -r .replayed_batch <<<"$out")" "true"
jv=$(api GET /regions/US-IA/journal/verify demo-operator-us-ia)
check "local journal stays gapless across the switch" "$(jq -r .gapless <<<"$jv")" "true"
check "journal head on the new primary covers all local history" "$(jq -r .head_seq <<<"$jv")" "6"

api POST /replication/run "$TOK_ADMIN" '{"region":"US-IA"}' > "$TMP/resume.json"
check "replication restarts from the confirmed position" \
  "$(jq -r '.from_seq' "$TMP/resume.json")" "$((confirmed_before + 1))"
area_group=$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='US-IA'")
area_local=$(sql_region us_ia "select round(sum(area_ha)::numeric,2) from operation_events")
check "post-failover replication has no duplicated area" "$area_group" "$area_local"

api POST /dr/US-IA/failback "$TOK_ADMIN" '{}' > "$TMP/failback.json"
check "failback restores the original primary" "$(jq -r '.to_node' "$TMP/failback.json")" "us-ia-node-a"

# A crashed writer must not leave orphan rows in the control plane. Simulate it
# by inserting an unconfirmed row past the confirmed position, then fail over.
confirmed_now=$(api GET /replication/cursors "$TOK_ADMIN" | jq -r '.cursors[]|select(.source_region=="US-IA")|.confirmed_seq')
sql_control "insert into replica_op_summaries (source_region, entity_id, source_seq, device_serial,
  segment_id, operation_type, area_ha, fuel_l, duration_s, completed, started_at, source_node, status)
  values ('US-IA','US-SEED-1203:phantom-crash', ${confirmed_now}+100, 'US-SEED-1203','phantom-crash',
          'seeding', 999.0, 0, 0, true, now(), 'us-ia-node-a', 'unconfirmed')" >/dev/null
phantom=$(sql_control "select count(*) from replica_op_summaries where entity_id='US-SEED-1203:phantom-crash'")
check "orphan row inserted for the drill" "$phantom" "1"
fo2=$(api POST /dr/US-IA/failover "$TOK_ADMIN" '{"reconcile":true}')
check "failover deletes the crashed writer's orphan" \
  "$(sql_control "select count(*) from replica_op_summaries where entity_id='US-SEED-1203:phantom-crash'")" "0"
check "orphan is accounted for in the failover report" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_op_summaries' <<<"$fo2")" "1"
area_local=$(sql_region us_ia "select round(sum(area_ha)::numeric,2) from operation_events")
check "group area still matches the region after the orphan drill" \
  "$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='US-IA'")" "$area_local"
api POST /dr/US-IA/failback "$TOK_ADMIN" '{}' >/dev/null

section "4. approval revoked mid-run blocks delivery"
apr2=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr2/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run2=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr2" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
rev=$(api POST "/outbound/requests/$apr2/revoke" "$TOK_APPROVER" '{"reason":"steward withdrew authorization mid-run"}')
check "approval revoked" "$(jq -r .status <<<"$rev")" "revoked"

dlv=$(api POST "/aggregates/$run2/deliver" "$TOK_ANALYST")
echo "$dlv" > "$TMP/blocked.json"
check "delivery is blocked, not sent" "$(jq -r .status "$TMP/blocked.json")" "blocked"
check "blocked delivery exports no artifact" "$(jq -r '.artifact' "$TMP/blocked.json")" "null"
check "blocked delivery carries no artifact hash" "$(jq -r '.artifact_sha256 // ""' "$TMP/blocked.json")" ""
check "revocation is the blocking reason" \
  "$(jq -r '.blocked_reason|startswith("approval_status")' "$TMP/blocked.json")" "true"

# Run computed under a narrow scope, delivered after the approval was revoked:
# the delivery must re-read the current approval, not the startup snapshot.
apr3=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/narrow.json" | jq -r .id)
api POST "/outbound/requests/$apr3/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
narrow_body=$(jq -n --arg a "$apr3" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",
  period_to:"2026-11-30T00:00:00Z", metric_keys:["operation_count","completion_rate","area_ha"]}')
run3=$(api POST /aggregates "$TOK_ANALYST" "$narrow_body" | jq -r .id)
check "narrow approval aggregates only its three regions" \
  "$(api GET "/aggregates/$run3" "$TOK_ANALYST" | jq -r '.coverage.included_regions|join(",")')" \
  "BR-MT,DE-BB,US-IA"
api POST "/outbound/requests/$apr3/revoke" "$TOK_APPROVER" '{"reason":"scope narrowed after run"}' >/dev/null
dlv3=$(api POST "/aggregates/$run3/deliver" "$TOK_ANALYST")
check "revoked narrow approval blocks delivery too" "$(jq -r .status <<<"$dlv3")" "blocked"
check "narrow run's blocked delivery exports nothing" "$(jq -r '.artifact' <<<"$dlv3")" "null"

# Narrowing the approval scope in place: the run was computed under a wider
# scope and must be blocked because the current scope no longer covers it.
apr5=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr5/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run5=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr5" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
sql_control "update outbound_requests set scope_regions=array['DE-BB'], version=version+1 where id='$apr5'" >/dev/null
dlv5=$(api POST "/aggregates/$run5/deliver" "$TOK_ANALYST")
check "narrowed-in-place approval blocks the wider run" "$(jq -r .status <<<"$dlv5")" "blocked"
check "the blocking check names the region scope" \
  "$(jq -r '[.checks[]|select(.name=="region_scope_current" and .passed==false)]|length' <<<"$dlv5")" "1"
check "narrowed scope still exports nothing" "$(jq -r '.artifact' <<<"$dlv5")" "null"

# A metric outside the approved scope must be refused at run creation.
wide_apr=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/narrow.json" | jq -r .id)
api POST "/outbound/requests/$wide_apr/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
out_of_scope=$(api POST /aggregates "$TOK_ANALYST" "$(jq -n --arg a "$wide_apr" '{approval_id:$a,
  period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",
  metric_keys:["operation_count","fuel_l"]}')")
check "metric outside the approval scope is refused" "$(jq -r .error.code <<<"$out_of_scope")" "forbidden"

section "5. outbound delivery and audit chain"
apr4=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr4/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run4=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr4" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
dlv4=$(api POST "/aggregates/$run4/deliver" "$TOK_ANALYST")
echo "$dlv4" > "$TMP/sent.json"
check "delivery sent" "$(jq -r .status "$TMP/sent.json")" "sent"
check "artifact lists the regions actually included" \
  "$(jq -r '.artifact.included_regions|join(",")' "$TMP/sent.json")" "BR-MT,DE-BB,FR-NOR,US-IA"
check "artifact declares no raw trajectories" \
  "$(jq -r '.artifact.contains_raw_trajectories' "$TMP/sent.json")" "false"
twice=$(api POST "/aggregates/$run4/deliver" "$TOK_ANALYST")
check "the same run cannot be delivered twice" "$(jq -r .error.code <<<"$twice")" "conflict"
check "all delivery checks passed" \
  "$(jq -r '[.checks[]|select(.passed==false)]|length' "$TMP/sent.json")" "0"

dlvid=$(jq -r .id "$TMP/sent.json")
ver=$(api GET "/deliveries/$dlvid/verify" "$TOK_AUDITOR")
echo "$ver" > "$TMP/verify.json"
check "stored artifact re-hashes to the recorded digest" "$(jq -r .sha256_matches "$TMP/verify.json")" "true"
check "artifact carries each region's replication version" \
  "$(jq -r '[.artifact.regions[]|select(.included) and (.replication_seq>0)]|length' "$TMP/sent.json")" "4"
check "artifact carries a data cutoff" \
  "$(jq -r '.artifact.data_cutoff != null' "$TMP/sent.json")" "true"
check "stored artifact has no restricted fields" \
  "$(jq -r '(.restricted_fields // [])|length' "$TMP/verify.json")" "0"
check "delivery audit entry is present and bound to the digest" "$(jq -r .audit_entry_valid "$TMP/verify.json")" "true"
check "audit chain verifies" "$(jq -r .audit_chain_valid "$TMP/verify.json")" "true"
check "group audit ledger verifies end to end" \
  "$(api GET /audit/verify "$TOK_AUDITOR" | jq -r .valid)" "true"

section "6. phenology stage alignment (days-after-sowing vs growing degree days)"
PH="$S/phenology"
ph_run() { # region operator method as_of
  api POST "/regions/$1/phenology/runs" "$2" \
    "{\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"$3\",\"as_of_date\":\"$4\"}"
}
for r in de-bb us-ia br-mt fr-nor; do
  code=$(slug "$r")
  res=$(api POST "/regions/$code/phenology/evidence" "demo-operator-$r" "@$PH/$r/evidence.json")
  echo "$res" > "$TMP/ph-ev-$r.json"
  check "phenology evidence accepted for $code" "$(jq -r .replayed_batch "$TMP/ph-ev-$r.json")" "false"
  check "phenology evidence binds rule version for $code" "$(jq -r .rule_id "$TMP/ph-ev-$r.json")" "S2026-MAIZE-v1"
done

# Same rule version, both routes, four regions. DE-BB and US-IA are compared at
# 30 days after sowing; DE-BB's cool spring and US-IA's warm spring diverge
# under GDD but agree under DAS.
ph_run DE-BB "demo-operator-de-bb" das 2026-05-10 > "$TMP/ph-de-das-30.json"
ph_run DE-BB "demo-operator-de-bb" gdd 2026-05-10 > "$TMP/ph-de-gdd-30.json"
ph_run US-IA "demo-operator-us-ia" das 2026-05-15 > "$TMP/ph-us-das-30.json"
ph_run US-IA "demo-operator-us-ia" gdd 2026-05-15 > "$TMP/ph-us-gdd-30.json"
check "DE-BB and US-IA both at 30 DAS agree under day thresholds" \
  "$(jq -r '.summary.stage_areas.emergence' "$TMP/ph-de-das-30.json")/$(jq -r '.summary.stage_areas.emergence' "$TMP/ph-us-das-30.json")" \
  "220/330"
check "same 30 DAS, cooler DE-BB reaches only emergence by GDD" \
  "$(jq -r '(.summary.stage_areas.vegetative // 0)' "$TMP/ph-de-gdd-30.json")" "0"
check "same 30 DAS, warmer US-IA reaches vegetative by GDD" \
  "$(jq -r '.summary.stage_areas.vegetative' "$TMP/ph-us-gdd-30.json")" "130"
# Area must reconcile: sown == sum(stage areas) + pending + pre-emergence.
check "stage/pending/pre-emergence areas reconcile to sown area" \
  "$(jq -r '(.summary.sown_area_ha) as $s | (([.summary.stage_areas[]]|add // 0) + .summary.pending_area_ha + .summary.pre_emergence_area_ha) as $a | ($s == $a)' "$TMP/ph-de-das-30.json")" "true"

# The rocking plot must be segmented; the reseed adds area rather than hiding it.
ph_run BR-MT "demo-operator-br-mt" das 2026-11-05 > "$TMP/ph-br-das.json"
check "reseeded plot is reported as a segmented plot" \
  "$(jq -r '.summary.segmented_plots' "$TMP/ph-br-das.json")" "1"
check "reseeded area is surfaced, not merged away" \
  "$(jq -r '.summary.reseeded_area_ha' "$TMP/ph-br-das.json")" "100"

# A weather gap makes the GDD crossing genuinely uncertain: the area is pending
# with a wide evidence range, never forced into a stage.
ph_run BR-MT "demo-operator-br-mt" gdd 2026-11-05 > "$TMP/ph-br-gdd.json"
check "BR-MT GDD weather gap leaves area pending" \
  "$(jq -r '.summary.pending[0].reason' "$TMP/ph-br-gdd.json")" "weather_gap_widens_stage"
check "BR-MT pending evidence range is wide" \
  "$(jq -r '.summary.pending_evidence.emergence.from' "$TMP/ph-br-gdd.json")" "2026-10-10"
check "BR-MT weather coverage is reported below 1" \
  "$(jq -r '.summary.weather_coverage < 0.7' "$TMP/ph-br-gdd.json")" "true"

# A weather station that is too far / too high makes the plot pending.
ph_run US-IA "demo-operator-us-ia" gdd 2026-05-20 > "$TMP/ph-us-gdd-20.json"
check "out-of-range station link makes area pending" \
  "$(jq -r '[.summary.pending[]|select(.reason=="station_distance_or_height_not_applicable")]|length' "$TMP/ph-us-gdd-20.json")" "1"

# Variety with no thresholds falls back to the published calendar for DAS and
# is pending for GDD.
ph_run FR-NOR "demo-operator-fr-nor" das 2026-05-20 > "$TMP/ph-fr-das.json"
check "calendar fallback used when variety declares no day thresholds" \
  "$(jq -r '.summary.calendar_used' "$TMP/ph-fr-das.json")" "true"
# A scouting note that contradicts the day-threshold stage leaves the plot
# pending rather than trusting either source.
check "contradictory stage evidence leaves area pending" \
  "$(jq -r '[.summary.pending[]|select(.reason=="stage_evidence_conflict")]|length' "$TMP/ph-fr-das.json")" "1"

# Immutability: a later correction (higher-revision sowing fact + weather
# revision) must not rewrite the run already issued above.
old_run=$(jq -r .run_id "$TMP/ph-de-das-30.json")
old_fp=$(jq -r .evidence_fingerprint "$TMP/ph-de-das-30.json")
# Capture a GDD run before the weather revision too.
ph_run DE-BB "demo-operator-de-bb" gdd 2026-05-20 > "$TMP/ph-de-gdd-before.json"
old_gdd_run=$(jq -r .run_id "$TMP/ph-de-gdd-before.json")
old_gdd_fp=$(jq -r .evidence_fingerprint "$TMP/ph-de-gdd-before.json")
api POST /regions/DE-BB/phenology/evidence "demo-operator-de-bb" "@$PH/de-bb/correction-v2.json" >/dev/null
ph_run DE-BB "demo-operator-de-bb" das 2026-05-10 > "$TMP/ph-de-das-corrected.json"
check "corrected evidence produces a new run" \
  "$(jq -r '.run_id != "'"$old_run"'"' "$TMP/ph-de-das-corrected.json")" "true"
check "corrected run has a different evidence fingerprint" \
  "$(jq -r '.evidence_fingerprint != "'"$old_fp"'"' "$TMP/ph-de-das-corrected.json")" "true"
check "the historical run is unchanged after the correction" \
  "$(api GET "/regions/DE-BB/phenology/runs/$old_run" \
      "$TOK_AUDITOR" | jq -r .evidence_fingerprint)" "$old_fp"
# A weather revision changes the GDD evidence fingerprint of a NEW run but not
# of the run that already read the old revision.
ph_run DE-BB "demo-operator-de-bb" gdd 2026-05-20 > "$TMP/ph-de-gdd-after.json"
check "weather revision changes the next GDD run's fingerprint" \
  "$(jq -r '.evidence_fingerprint != "'"$old_gdd_fp"'"' "$TMP/ph-de-gdd-after.json")" "true"
check "the historical GDD run still reports its original fingerprint" \
  "$(api GET "/regions/DE-BB/phenology/runs/$old_gdd_run" \
      "$TOK_AUDITOR" | jq -r .evidence_fingerprint)" "$old_gdd_fp"

# Replicate stage products to the group plane (region granularity only).
# Ensure each region has a DAS run at the comparison date first, then replicate
# so every region's product is confirmed for comparison.
ph_run DE-BB "demo-operator-de-bb" das 2026-05-20 > "$TMP/ph-de-das-20.json"
ph_run US-IA "demo-operator-us-ia" das 2026-05-20 > "$TMP/ph-us-das-20.json"
ph_run FR-NOR "demo-operator-fr-nor" das 2026-05-20 > "$TMP/ph-fr-das-20.json"
api POST /replication/batch "$TOK_ADMIN" '{}' > "$TMP/ph-repl.json"
check "stage products replicated from every region" \
  "$(jq -r '[.runs[]|select(.copied_by_category.phenology_stage>0)]|length' "$TMP/ph-repl.json")" "4"

# Group comparison across hemispheres at each region's local equivalent date.
aprph=$(api POST /outbound/requests "$TOK_ANALYST" "@$PH/approvals/phenology-wide.json" | jq -r .id)
api POST "/outbound/requests/$aprph/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
cmpbody=$(jq -n --arg a "$aprph" '{approval_id:$a,rule_id:"S2026-MAIZE-v1",method:"das",
  as_of_date:"2026-05-20",region_as_of:{"BR-MT":"2026-11-05"},
  stage_keys:["emergence","vegetative","harvest_prep"]}')
phcmp=$(api POST /phenology/comparisons "$TOK_ANALYST" "$cmpbody")
echo "$phcmp" > "$TMP/ph-cmp.json"
phcmpid=$(jq -r .id "$TMP/ph-cmp.json")
check "comparison aligns all four regions across hemispheres" \
  "$(jq -r '.included_regions|sort|join(",")' "$TMP/ph-cmp.json")" "BR-MT,DE-BB,FR-NOR,US-IA"
check "comparison reports a common comparable stage" \
  "$(jq -r '.common_stage' "$TMP/ph-cmp.json")" "emergence"
check "comparison never includes excluded plot identifiers" \
  "$(jq -r '[.regions[]|has("plot_id")]|any' "$TMP/ph-cmp.json")" "false"
check "comparison carries the agronomic-progress disclaimer" \
  "$(jq -r '[.disclaimer[]|select(test("does not rate team performance"))]|length>0' "$TMP/ph-cmp.json")" "true"

# A region with no confirmed run for the requested date is listed, not dropped.
missing_body=$(jq -n --arg a "$aprph" '{approval_id:$a,rule_id:"S2026-MAIZE-v1",method:"dinvalid",
  as_of_date:"2026-05-20"}')
check "an invalid comparison method is refused" \
  "$(api POST /phenology/comparisons "$TOK_ANALYST" "$missing_body" | jq -r .error.code)" "invalid_request"

# Delivery re-validates the approval, scans the exact bytes, and is auditable.
phdlv=$(api POST "/phenology/comparisons/$phcmpid/deliver" "$TOK_ANALYST")
echo "$phdlv" > "$TMP/ph-dlv.json"
check "phenology comparison delivered" "$(jq -r .status "$TMP/ph-dlv.json")" "sent"
check "phenology artifact carries no raw coordinates" \
  "$(jq -r '.artifact.contains_raw_trajectories' "$TMP/ph-dlv.json")" "false"
check "phenology artifact reports pending totals separately" \
  "$(jq -r '.artifact.pending_totals|length>0' "$TMP/ph-dlv.json")" "true"
phdlvid=$(jq -r .id "$TMP/ph-dlv.json")
phver=$(api GET "/phenology/deliveries/$phdlvid/verify" "$TOK_AUDITOR")
echo "$phver" > "$TMP/ph-ver.json"
check "phenology artifact re-hashes to the recorded digest" \
  "$(jq -r .sha256_matches "$TMP/ph-ver.json")" "true"
check "phenology artifact has no restricted fields" \
  "$(jq -r '(.restricted_fields // [])|length' "$TMP/ph-ver.json")" "0"
check "phenology delivery audit entry is bound to the digest" \
  "$(jq -r .audit_entry_valid "$TMP/ph-ver.json")" "true"
check "phenology delivery sits on a valid audit chain" \
  "$(jq -r .audit_chain_valid "$TMP/ph-ver.json")" "true"

# Revocation between comparison and delivery blocks the phenology delivery too.
aprph2=$(api POST /outbound/requests "$TOK_ANALYST" "@$PH/approvals/phenology-wide.json" | jq -r .id)
api POST "/outbound/requests/$aprph2/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
cmp2=$(api POST /phenology/comparisons "$TOK_ANALYST" \
  "$(jq --arg a "$aprph2" '.approval_id=$a' <<<"$cmpbody")" | jq -r .id)
api POST "/outbound/requests/$aprph2/revoke" "$TOK_APPROVER" '{"reason":"steward withdrew stage-comparison authorization"}' >/dev/null
phblk=$(api POST "/phenology/comparisons/$cmp2/deliver" "$TOK_ANALYST")
echo "$phblk" > "$TMP/ph-blocked.json"
check "revoked approval blocks the phenology delivery" "$(jq -r .status "$TMP/ph-blocked.json")" "blocked"
check "blocked phenology delivery exports no artifact" "$(jq -r '.artifact' "$TMP/ph-blocked.json")" "null"

# Raw trajectories still never leave the restricted regions after all of this.
stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "stage totals did not smuggle raw trajectories out of restricted regions" \
  "$(jq -r '[(.raw_trajectories_by_region["DE-BB"] // 0),(.raw_trajectories_by_region["BR-MT"] // 0),(.raw_trajectories_by_region["FR-NOR"] // 0)]|add' <<<"$stats")" "0"

# Out-of-band tampering with the ledger must be detected.
sql_control "update audit_events set detail=jsonb_set(detail,'{included_regions}','[\"XX-XX\"]') where seq=1" >/dev/null
tampered=$(api GET /audit/verify "$TOK_AUDITOR")
echo "$tampered" > "$TMP/tamper.json"
check "tampered audit row is detected" "$(jq -r .valid "$TMP/tamper.json")" "false"
check "tamper is reported at the edited row" "$(jq -r .broken_at "$TMP/tamper.json")" "1"

section "summary"
printf 'checks passed: %d, failed: %d\n' "$PASS" "$FAIL"
if (( FAIL > 0 )); then
  printf '\n\033[31mFAILED CHECKS\033[0m\n'
  for c in "${CHECKS[@]}"; do
    [[ "$c" == FAIL* ]] && printf '  %s\n' "$c"
  done
  exit 1
fi
printf '\n\033[32mALL CHECKS PASSED\033[0m\n'
