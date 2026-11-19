-- Control-plane schema: region registry, residency policy, replication
-- positions, group-visible replicas, cross-border approvals, aggregate runs,
-- deliveries and the tamper-evident audit ledger.
-- This database intentionally has NO raw trajectory data except for replicas
-- that residency policy explicitly allows to leave a region.

create table if not exists regions (
  code       text primary key,
  name       text not null,
  country    text not null,
  timezone   text not null,
  db_name    text not null unique,
  status     text not null default 'active' check (status in ('active','inactive')),
  created_at timestamptz not null default now()
);

create table if not exists residency_policies (
  region_code      text not null references regions(code) on delete cascade,
  category         text not null check (category in ('raw_trajectory','operation_summary','device_alarm','aggregate_metric')),
  outbound_allowed boolean not null,
  rationale        text not null default '',
  updated_at       timestamptz not null default now(),
  primary key (region_code, category)
);

-- Disaster-recovery register. last_confirmed_seq is the only position a new
-- primary is allowed to resume from after a failover.
create table if not exists dr_registry (
  region_code        text primary key references regions(code) on delete cascade,
  primary_node       text not null,
  standby_node       text not null,
  active_node        text not null,
  epoch              bigint not null default 1,
  last_confirmed_seq bigint not null default 0,
  last_confirmed_at  timestamptz,
  status             text not null default 'healthy' check (status in ('healthy','failed_over','reconciling')),
  updated_at         timestamptz not null default now()
);

create table if not exists dr_events (
  id            bigserial primary key,
  region_code   text not null references regions(code) on delete cascade,
  event_type    text not null,
  from_node     text,
  to_node       text,
  confirmed_seq bigint not null default 0,
  epoch         bigint not null default 0,
  detail        jsonb not null default '{}'::jsonb,
  created_at    timestamptz not null default now()
);

create table if not exists replication_links (
  region_code text primary key references regions(code) on delete cascade,
  status      text not null default 'online' check (status in ('online','offline')),
  reason      text not null default '',
  updated_at  timestamptz not null default now()
);

create table if not exists replication_cursors (
  source_region   text primary key references regions(code) on delete cascade,
  last_seq        bigint not null default 0,
  confirmed_seq   bigint not null default 0,
  status          text not null default 'idle' check (status in ('idle','failed','pending_reconcile')),
  retry_count     integer not null default 0,
  copied_total    bigint not null default 0,
  blocked_total   bigint not null default 0,
  last_attempt_at timestamptz,
  last_success_at timestamptz,
  last_error      text,
  updated_at      timestamptz not null default now()
);

create table if not exists replication_runs (
  id                  text primary key,
  source_region       text not null references regions(code) on delete cascade,
  from_seq            bigint not null default 0,
  to_seq              bigint not null default 0,
  copied_by_category  jsonb not null default '{}'::jsonb,
  blocked_by_category jsonb not null default '{}'::jsonb,
  status              text not null check (status in ('succeeded','failed','noop')),
  fault               text not null default 'none',
  error               text,
  started_at          timestamptz not null,
  finished_at         timestamptz not null
);
create index if not exists replication_runs_region_idx on replication_runs (source_region, started_at desc);

create table if not exists replica_op_summaries (
  source_region  text not null,
  entity_id      text not null,
  source_seq     bigint not null,
  device_serial  text not null,
  segment_id     text not null,
  operation_type text not null,
  field_id       text not null default '',
  area_ha        double precision not null,
  fuel_l         double precision not null,
  duration_s     integer not null,
  completed      boolean not null,
  started_at     timestamptz not null,
  ended_at       timestamptz,
  source_node    text not null,
  status         text not null check (status in ('confirmed','unconfirmed')),
  copied_at      timestamptz not null default now(),
  primary key (source_region, entity_id)
);
create unique index if not exists replica_op_summaries_segment_uidx
  on replica_op_summaries (source_region, device_serial, segment_id);
create index if not exists replica_op_summaries_started_idx
  on replica_op_summaries (source_region, started_at);

create table if not exists replica_trajectories (
  source_region text not null,
  entity_id     text not null,
  source_seq    bigint not null,
  device_serial text not null,
  segment_id    text not null,
  point_count   integer not null,
  points        jsonb not null,
  source_node   text not null,
  status        text not null check (status in ('confirmed','unconfirmed')),
  copied_at     timestamptz not null default now(),
  primary key (source_region, entity_id)
);

create table if not exists replica_alarms (
  source_region text not null,
  entity_id     text not null,
  source_seq    bigint not null,
  device_serial text not null,
  segment_id    text not null,
  code          text not null,
  severity      text not null,
  occurred_at   timestamptz not null,
  source_node   text not null,
  status        text not null check (status in ('confirmed','unconfirmed')),
  copied_at     timestamptz not null default now(),
  primary key (source_region, entity_id)
);

create table if not exists replica_metrics (
  source_region text not null,
  snapshot_id   text not null,
  source_seq    bigint not null,
  source_epoch  bigint not null default 0,
  period_from   timestamptz not null,
  period_to     timestamptz not null,
  data_cutoff   timestamptz not null,
  computed_at   timestamptz not null,
  metrics       jsonb not null,
  source_node   text not null,
  status        text not null check (status in ('confirmed','unconfirmed')),
  copied_at     timestamptz not null default now(),
  primary key (source_region, snapshot_id)
);

create table if not exists outbound_requests (
  id            text primary key,
  purpose       text not null,
  recipient     text not null,
  scope_regions text[] not null,
  scope_metrics text[] not null,
  status        text not null check (status in ('pending','approved','rejected','revoked')),
  version       integer not null default 1,
  valid_from    timestamptz,
  valid_until   timestamptz,
  requested_by  text not null,
  decided_by    text,
  decided_at    timestamptz,
  decision_note text,
  revoked_by    text,
  revoked_at    timestamptz,
  revoke_reason text,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

create table if not exists aggregate_runs (
  id             text primary key,
  approval_id    text not null references outbound_requests(id),
  period_from    timestamptz not null,
  period_to      timestamptz not null,
  metric_keys    text[] not null,
  status         text not null check (status in ('computed','delivered','blocked')),
  scope_snapshot jsonb not null,
  coverage       jsonb not null,
  result         jsonb not null,
  data_cutoff    timestamptz,
  approval_version integer not null default 1,
  requested_by   text not null,
  created_at     timestamptz not null default now()
);

create table if not exists aggregate_run_regions (
  run_id           text not null references aggregate_runs(id) on delete cascade,
  region_code      text not null,
  included         boolean not null,
  exclusion_reason text,
  replication_seq  bigint,
  snapshot_id      text,
  data_cutoff      timestamptz,
  metrics          jsonb,
  primary key (run_id, region_code)
);

-- The artifact is stored as the exact JSON text that was hashed and sent, so
-- verification can re-hash the same bytes. (jsonb would normalise key order
-- and number formatting and break the hash.)
do $$
begin
  if exists (select 1 from information_schema.columns
             where table_name='deliveries' and column_name='artifact'
               and data_type='jsonb') then
    alter table deliveries alter column artifact type text using artifact::text;
  end if;
end $$;

create table if not exists deliveries (
  id              text primary key,
  run_id          text not null references aggregate_runs(id),
  approval_id     text not null,
  status          text not null check (status in ('sent','blocked')),
  channel         text not null default 'secure_store',
  checks          jsonb not null default '[]'::jsonb,
  blocked_reason  text,
  artifact        text,
  artifact_sha256 text,
  audit_seq       bigint,
  requested_by    text not null,
  created_at      timestamptz not null default now(),
  sent_at         timestamptz
);

create table if not exists audit_events (
  seq          bigserial primary key,
  occurred_at  timestamptz not null,
  actor        text not null,
  role         text not null,
  action       text not null,
  subject_type text not null default '',
  subject_id   text not null default '',
  region_code  text not null default '',
  decision     text not null default 'allow',
  detail       jsonb not null default '{}'::jsonb,
  prev_hash    text not null,
  hash         text not null
);
create index if not exists audit_events_occurred_idx on audit_events (occurred_at desc);

-- ---------------------------------------------------------------------------
-- Phenology stage alignment at group level.
-- The region publishes only its region-granularity stage product (area by
-- stage, pending range, evidence window). Plot identifiers and geometry never
-- reach the control plane.
-- ---------------------------------------------------------------------------

-- Extend the residency category allowlist idempotently for existing installs.
-- New installs already declare the wider list in the create table above; this
-- only rewrites an older constraint that predates phenology_stage.
do $$
declare
  def text;
begin
  select pg_get_constraintdef(c.oid) into def
  from pg_constraint c
  join pg_class t on t.oid = c.conrelid
  where t.relname = 'residency_policies' and c.contype = 'c'
    and pg_get_constraintdef(c.oid) like '%raw_trajectory%'
  limit 1;
  if def is not null and position('phenology_stage' in def) = 0 then
    alter table residency_policies drop constraint residency_policies_category_check;
    alter table residency_policies add constraint residency_policies_category_check
      check (category in ('raw_trajectory','operation_summary','device_alarm',
                          'aggregate_metric','phenology_stage'));
  end if;
end $$;

create table if not exists replica_phenology_products (
  source_region  text not null,
  entity_id      text not null,
  source_seq     bigint not null,
  source_epoch   bigint not null default 0,
  run_id         text not null,
  rule_id        text not null,
  method         text not null,
  as_of_date     date not null,
  product        jsonb not null,
  source_node    text not null,
  status         text not null check (status in ('confirmed','unconfirmed')),
  copied_at      timestamptz not null default now(),
  primary key (source_region, entity_id)
);
create unique index if not exists replica_phenology_products_run_uidx
  on replica_phenology_products (source_region, run_id);

create table if not exists phenology_comparisons (
  id                        text primary key,
  approval_id               text not null references outbound_requests(id),
  approval_version          integer not null default 1,
  rule_id                   text not null,
  season_id                 text not null,
  rule_version              text not null,
  crop                      text not null,
  method                    text not null check (method in ('das','gdd')),
  as_of_date                date not null,
  stage_keys                text[] not null,
  status                    text not null check (status in ('computed','delivered','blocked')),
  included_regions          text[] not null default '{}',
  excluded_regions          text[] not null default '{}',
  regions                   jsonb not null default '[]'::jsonb,
  common_stage              text not null default '',
  common_comparable_regions text[] not null default '{}',
  stage_totals              jsonb not null default '{}'::jsonb,
  pending_totals            jsonb not null default '{}'::jsonb,
  requested_by              text not null,
  created_at                timestamptz not null default now()
);

create table if not exists phenology_deliveries (
  id              text primary key,
  comparison_id   text not null references phenology_comparisons(id),
  approval_id     text not null,
  status          text not null check (status in ('sent','blocked')),
  channel         text not null default 'secure_store',
  checks          jsonb not null default '[]'::jsonb,
  blocked_reason  text,
  artifact        text,
  artifact_sha256 text,
  audit_seq       bigint,
  requested_by    text not null,
  created_at      timestamptz not null default now(),
  sent_at         timestamptz
);
