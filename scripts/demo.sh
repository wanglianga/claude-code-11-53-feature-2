#!/usr/bin/env bash
# Human-oriented walkthrough: prints the group's state at each stage so the
# behaviour is readable, without assertions. Use scripts/replay.sh to verify.
set -euo pipefail
BASE="${1:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
S="$ROOT/samples"
A="Authorization: Bearer demo-admin-token"
AN="Authorization: Bearer demo-analyst-token"
AP="Authorization: Bearer demo-approver-token"
AU="Authorization: Bearer demo-auditor-token"
post() { curl -sS -XPOST -H "$2" -H 'Content-Type: application/json' ${3:+--data-binary "$3"} "$BASE$1"; }
get()  { curl -sS -H "$2" "$BASE$1"; }

hr() { printf '\n\033[1m--- %s ---\033[0m\n' "$1"; }

hr "reset"
post /demo/reset "$A" | jq -c .

hr "each region uploads locally (8 work segments each incl. raw trajectories)"
for r in de-bb br-mt us-ia fr-nor; do
  code=$(echo "$r" | tr 'a-z' 'A-Z')
  post "/regions/$code/ingest" "Authorization: Bearer demo-operator-$r" "@$S/regions/$r.json" \
    | jq -c '{region,accepted,duplicates,first_journal_seq,last_journal_seq,active_node}'
done

hr "replication: summaries/alarms leave every region; raw trajectories only where policy allows"
post /replication/batch "$A" '{}' | jq -c '.runs[]|{source_region,status,copied:.copied_by_category,blocked:.blocked_by_category}'
get /replication/replicas "$A" | jq -c .

hr "BR-MT link goes down; the farm keeps working and the journal stays continuous"
post /replication/links/BR-MT "$A" '{"status":"offline","reason":"demo: backbone cut"}' >/dev/null
post /regions/BR-MT/ingest "Authorization: Bearer demo-operator-br-mt" "@$S/offline/br-mt-offline-backfill.json" \
  | jq -c '{accepted,first_journal_seq,last_journal_seq}'
get /regions/BR-MT/status "Authorization: Bearer demo-operator-br-mt" | jq -c '{journal_head,journal_entries,operation_count,pending_outbox,replication_link,handover_ready}'
get /regions/BR-MT/journal/verify "Authorization: Bearer demo-operator-br-mt" | jq -c .

hr "the group report says exactly which region is missing"
APR=$(post /outbound/requests "$AN" "@$S/approvals/wide.json" | jq -r .id)
post "/outbound/requests/$APR/decide" "$AP" '{"decision":"approved","note":"demo"}' >/dev/null
RUN=$(post /aggregates "$AN" "$(jq -n --arg a "$APR" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","completed_count","completion_rate","area_ha","fuel_l","duration_hours"]}')" | jq -r .id)
get "/aggregates/$RUN" "$AN" | jq -c '{included:.coverage.included_regions,excluded:.coverage.excluded_regions,delayed:.coverage.stale_regions,cutoff:.coverage.data_cutoff,totals:.result}'

hr "link restored; overlapping retry upload dedupes and replication confirms the backlog"
post /replication/links/BR-MT "$A" '{"status":"online","reason":""}' >/dev/null
post /regions/BR-MT/ingest "Authorization: Bearer demo-operator-br-mt" "@$S/offline/br-mt-retry-overlap.json" \
  | jq -c '{accepted,duplicates,first_journal_seq,last_journal_seq}'
post /replication/run "$A" '{"region":"BR-MT"}' | jq -c '{status,copied:.copied_by_category,blocked:.blocked_by_category}'
get /replication/cursors "$A" | jq -c '.cursors[]|select(.source_region=="BR-MT")|{scanned_seq,confirmed_seq,retry_count,copied_total,blocked_total}'

hr "approval revoked after the run: delivery is blocked and leaks nothing"
APR2=$(post /outbound/requests "$AN" "@$S/approvals/narrow.json" | jq -r .id)
post "/outbound/requests/$APR2/decide" "$AP" '{"decision":"approved"}' >/dev/null
RUN2=$(post /aggregates "$AN" "$(jq -n --arg a "$APR2" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","area_ha"]}')" | jq -r .id)
post "/outbound/requests/$APR2/revoke" "$AP" '{"reason":"withdrawn during delivery window"}' >/dev/null
post "/aggregates/$RUN2/deliver" "$AN" | jq -c '{status,blocked_reason,artifact,checks:[.checks[]|select(.passed==false)|.name]}'

hr "authorized delivery, then verify the audit chain around it"
APR3=$(post /outbound/requests "$AN" "@$S/approvals/wide.json" | jq -r .id)
post "/outbound/requests/$APR3/decide" "$AP" '{"decision":"approved"}' >/dev/null
RUN3=$(post /aggregates "$AN" "$(jq -n --arg a "$APR3" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","completion_rate","area_ha"]}')" | jq -r .id)
DLV=$(post "/aggregates/$RUN3/deliver" "$AN" | tee /tmp/dlv.json | jq -r .id)
jq -c '{status,sha:.artifact_sha256,included:.artifact.included_regions,excluded:.artifact.excluded_regions,data_cutoff:.artifact.data_cutoff,notes:.artifact.notes}' /tmp/dlv.json
get "/deliveries/$DLV/verify" "$AU" | jq -c .
get /audit/verify "$AU" | jq -c .

hr "primary/standby switch resumes at the confirmed position"
get /dr/US-IA "$A" | jq -c '.registry|{active_node,standby_node,epoch,last_confirmed_seq}'
post /dr/US-IA/failover "$A" '{"reconcile":true}' | jq -c '{from_node,to_node,epoch,confirmed_seq,discarded_unconfirmed_rows,resumed_from_seq}'
get /regions/US-IA/status "Authorization: Bearer demo-operator-us-ia" | jq -c '{active_node,epoch,journal_head,journal_entries,handover_ready}'
post /dr/US-IA/failback "$A" '{}' | jq -c '{from_node,to_node,epoch}'

hr "phenology: upload season evidence and derive stages in-region (DAS + GDD)"
PH="$S/phenology"
for r in de-bb us-ia br-mt fr-nor; do
  code=$(echo "$r" | tr 'a-z' 'A-Z')
  post "/regions/$code/phenology/evidence" "Authorization: Bearer demo-operator-$r" "@$PH/$r/evidence.json" \
    | jq -c '{region,rule_id,counts}'
done
# DE-BB (cool) and US-IA (warm) at 30 days after sowing: same DAS, different GDD.
for spec in "DE-BB de-bb das 2026-05-10" "DE-BB de-bb gdd 2026-05-10" "US-IA us-ia das 2026-05-15" "US-IA us-ia gdd 2026-05-15"; do
  set -- $spec
  post "/regions/$1/phenology/runs" "Authorization: Bearer demo-operator-$2" \
    "{\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"$3\",\"as_of_date\":\"$4\"}" \
    | jq -c '{region_code,method,as_of_date,stages:.summary.stage_areas,pending:.summary.pending_area_ha}'
done
for spec in "DE-BB de-bb 2026-05-20" "US-IA us-ia 2026-05-20" "FR-NOR fr-nor 2026-05-20" "BR-MT br-mt 2026-11-05"; do
  set -- $spec
  post "/regions/$1/phenology/runs" "Authorization: Bearer demo-operator-$2" \
    "{\"rule_id\":\"S2026-MAIZE-v1\",\"method\":\"das\",\"as_of_date\":\"$3\"}" >/dev/null
done

hr "phenology: late correction issues a NEW run; the old run keeps its evidence"
OLD=$(post /regions/DE-BB/phenology/runs "Authorization: Bearer demo-operator-de-bb" \
  '{"rule_id":"S2026-MAIZE-v1","method":"das","as_of_date":"2026-05-20"}' | jq -r .run_id)
post /regions/DE-BB/phenology/evidence "Authorization: Bearer demo-operator-de-bb" "@$PH/de-bb/correction-v2.json" >/dev/null
post /regions/DE-BB/phenology/runs "Authorization: Bearer demo-operator-de-bb" \
  '{"rule_id":"S2026-MAIZE-v1","method":"das","as_of_date":"2026-05-20"}' >/dev/null
get "/regions/DE-BB/phenology/runs/$OLD" "Authorization: Bearer demo-operator-de-bb" \
  | jq -c '{run_id,evidence_fingerprint,stages:.summary.stage_areas}'

hr "phenology: replicate region-granularity stage products, align across hemispheres"
post /replication/batch "$A" '{}' >/dev/null
APRPH=$(post /outbound/requests "$AN" "@$PH/approvals/phenology-wide.json" | jq -r .id)
post "/outbound/requests/$APRPH/decide" "$AP" '{"decision":"approved"}' >/dev/null
CMP=$(post /phenology/comparisons "$AN" "$(jq -n --arg a "$APRPH" '{approval_id:$a,rule_id:"S2026-MAIZE-v1",
  method:"das",as_of_date:"2026-05-20",region_as_of:{"BR-MT":"2026-11-05"},
  stage_keys:["emergence","vegetative","harvest_prep"]}')")
echo "$CMP" | jq -c '{included:.included_regions,excluded:.excluded_regions,common:.common_stage,
  comparable:.common_comparable_regions,stage_totals,pending_totals}'
CID=$(echo "$CMP" | jq -r .id)
DLV=$(post "/phenology/comparisons/$CID/deliver" "$AN")
echo "$DLV" | jq -c '{status,sha:.artifact_sha256,coords:.artifact.contains_raw_trajectories,
  pending:.artifact.pending_totals,checks:[.checks[]|select(.passed==false)|.name]}'
DID=$(echo "$DLV" | jq -r .id)
get "/phenology/deliveries/$DID/verify" "$AU" | jq -c .
