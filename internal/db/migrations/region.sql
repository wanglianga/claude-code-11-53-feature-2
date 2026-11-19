-- Per-region operational schema. Each region has its own PostgreSQL database;
-- raw trajectories written here are never copied out unless residency policy
-- explicitly allows the raw_trajectory category to leave the region.
-- Local writes commit locally even while the inter-region link is down.

create table if not exists region_state (
  id                boolean primary key default true check (id),
  region_code       text not null,
  active_node       text not null,
  local_epoch       bigint not null default 1,
  next_journal_seq  bigint not null default 1,
  next_outbox_seq   bigint not null default 1,
  updated_at        timestamptz not null default now()
);

create table if not exists devices (
  device_serial  text primary key,
  device_type    text not null default 'seeder',
  model          text not null default '',
  first_seen_node text not null,
  created_at     timestamptz not null default now()
);

-- Local, authoritative operation records. Deduplication key is the device
-- serial plus the region that sourced the segment; the region is implicit in
-- the database boundary and explicit in every cross-region replica.
create table if not exists operation_events (
  id            text primary key,
  journal_seq   bigint not null unique,
  device_serial text not null,
  segment_id    text not null,
  operation_type text not null check (operation_type in ('seeding','harvesting','fertilizing')),
  field_id      text not null default '',
  area_ha       double precision not null check (area_ha >= 0),
  fuel_l        double precision not null default 0 check (fuel_l >= 0),
  duration_s    integer not null default 0 check (duration_s >= 0),
  completed     boolean not null default false,
  started_at    timestamptz not null,
  ended_at      timestamptz,
  point_count   integer not null default 0,
  points        jsonb not null default '[]'::jsonb,
  alarms        jsonb not null default '[]'::jsonb,
  upload_batch  text not null default '',
  source_node   text not null,
  source_epoch  bigint not null default 1,
  received_at   timestamptz not null default now(),
  unique (device_serial, segment_id)
);
create index if not exists operation_events_started_idx on operation_events (started_at);
create index if not exists operation_events_type_idx on operation_events (operation_type);

-- Gapless local journal. The next sequence is allocated under a row lock in
-- the same transaction as the operation insert, so a region that keeps
-- recording through an outage has a contiguous, provable local history.
create table if not exists journal_entries (
  seq          bigint primary key,
  entity_type  text not null,
  entity_id    text not null,
  source_node  text not null,
  created_at   timestamptz not null default now()
);

-- Transactional outbox. Sequence is gapless and allocated under the same row
-- lock as the journal entry. One operation fans out into a summary entry, a
-- raw trajectory entry and one entry per device alarm; replication filters
-- each category against residency policy.
create table if not exists outbox_events (
  seq          bigint primary key,
  entity_type  text not null,
  entity_id    text not null,
  category     text not null check (category in ('raw_trajectory','operation_summary','device_alarm','aggregate_metric','phenology_stage')),
  payload      jsonb not null,
  source_node  text not null,
  source_epoch bigint not null,
  created_at   timestamptz not null default now()
);
create index if not exists outbox_events_category_idx on outbox_events (category, seq);

-- In-region aggregate snapshots. Only these aggregates, never raw points, are
-- eligible for outbound reports.
create table if not exists metric_snapshots (
  snapshot_id  text primary key,
  period_from  timestamptz not null,
  period_to    timestamptz not null,
  data_cutoff  timestamptz not null,
  computed_at  timestamptz not null,
  metrics      jsonb not null,
  source_node  text not null,
  source_epoch bigint not null,
  outbox_seq   bigint not null
);
create index if not exists metric_snapshots_period_idx on metric_snapshots (period_from, period_to);

create table if not exists ingest_receipts (
  batch_id       text primary key,
  content_sha256 text not null,
  accepted       integer not null,
  duplicates     integer not null,
  first_journal  bigint,
  last_journal   bigint,
  created_at     timestamptz not null default now()
);

-- ---------------------------------------------------------------------------
-- Phenology stage alignment (in-region evidence and immutable stage runs).
--
-- All evidence is scoped by rule_id: a season rule version binds sowing
-- facts, the variety catalog, weather/calendar evidence, derivation and the
-- resulting region-stage product together. Runs are append-only; a late
-- sowing fact or a weather revision produces a NEW run and never rewrites an
-- existing one.
-- ---------------------------------------------------------------------------

create table if not exists season_rules (
  rule_id                     text primary key,
  season_id                   text not null,
  rule_version                text not null,
  crop                        text not null,
  hemisphere                  text not null,
  das_tolerance_days          integer not null default 3,
  weather_gap_pending_days    integer not null default 10,
  min_weather_coverage        double precision not null default 0.9,
  station_max_distance_km     double precision not null default 40,
  station_max_elevation_delta_m double precision not null default 120,
  missing_day_upper_fraction  double precision not null default 0.5,
  published_at                timestamptz not null,
  notes                       jsonb not null default '[]'::jsonb,
  created_at                  timestamptz not null default now()
);

create table if not exists phenology_varieties (
  rule_id        text not null,
  variety_code   text not null,
  crop           text not null,
  maturity_group text not null default '',
  das_thresholds jsonb not null default '{}'::jsonb,
  gdd            jsonb,
  updated_at     timestamptz not null default now(),
  primary key (rule_id, variety_code)
);

-- Plot declarations carry area but no geometry: the group product is region
-- granularity only.
create table if not exists phenology_plots (
  plot_id          text primary key,
  crop             text not null,
  declared_area_ha double precision not null check (declared_area_ha >= 0),
  note             text not null default '',
  received_at      timestamptz not null default now()
);

-- Sowing facts are versioned by fact_id + revision. A higher revision with
-- status 'active' supersedes lower revisions for future runs; historical runs
-- keep the revision they read.
create table if not exists sowing_facts (
  fact_id      text not null,
  revision     integer not null,
  rule_id      text not null,
  plot_id      text not null,
  segment_no   integer not null,
  crop         text not null,
  variety_code text not null default '',
  sown_on      date not null,
  area_ha      double precision not null check (area_ha >= 0),
  event_type   text not null check (event_type in ('initial','reseed')),
  evidence_ref text not null,
  status       text not null check (status in ('active','superseded','cancelled')),
  recorded_at  timestamptz not null,
  received_at  timestamptz not null default now(),
  primary key (fact_id, revision)
);
create index if not exists sowing_facts_rule_idx on sowing_facts (rule_id, plot_id);

create table if not exists stage_calendars (
  rule_id      text not null,
  calendar_id  text not null,
  crop         text not null,
  variety_code text not null default '',
  stage        text not null,
  start_day    integer not null,
  end_day      integer not null,
  source       text not null default '',
  published_at timestamptz not null,
  received_at  timestamptz not null default now(),
  primary key (rule_id, calendar_id)
);

create table if not exists weather_stations (
  rule_id     text not null,
  station_id  text not null,
  label       text not null default '',
  elevation_m double precision not null default 0,
  received_at timestamptz not null default now(),
  primary key (rule_id, station_id)
);

create table if not exists station_links (
  rule_id           text not null,
  plot_id           text not null,
  station_id        text not null,
  distance_km       double precision not null,
  elevation_delta_m double precision not null,
  received_at       timestamptz not null default now(),
  primary key (rule_id, plot_id)
);

-- One row per station-day revision. The highest revision with status
-- 'active' wins for a new run; historical runs retain the revision they used.
create table if not exists temperature_observations (
  rule_id     text not null,
  station_id  text not null,
  obs_date    date not null,
  revision    integer not null,
  tmin_c      double precision not null,
  tmax_c      double precision not null,
  source      text not null default '',
  status      text not null check (status in ('active','superseded','missing')),
  recorded_at timestamptz not null,
  received_at timestamptz not null default now(),
  primary key (rule_id, station_id, obs_date, revision)
);

create table if not exists stage_observations (
  rule_id        text not null,
  observation_id text not null,
  plot_id        text not null,
  segment_no     integer not null default 0,
  stage          text not null,
  observed_on    date not null,
  source         text not null default '',
  received_at    timestamptz not null default now(),
  primary key (rule_id, observation_id)
);

-- Immutable derivation runs. The evidence fingerprint pins exactly which
-- revisions of sowing facts and weather observations the run read, so a
-- later correction can be shown not to have altered this run.
create table if not exists phenology_runs (
  run_id               text primary key,
  rule_id              text not null,
  season_id            text not null,
  rule_version         text not null,
  crop                 text not null,
  method               text not null check (method in ('das','gdd')),
  as_of_date           date not null,
  status               text not null,
  evidence_fingerprint text not null,
  summary              jsonb not null,
  segments             jsonb not null default '[]'::jsonb,
  outbox_seq           bigint not null default 0,
  created_at           timestamptz not null default now()
);
create index if not exists phenology_runs_rule_idx on phenology_runs (rule_id, method, as_of_date desc);

create table if not exists phenology_ingest_receipts (
  batch_id       text primary key,
  content_sha256 text not null,
  rule_id        text not null,
  counts         jsonb not null default '{}'::jsonb,
  created_at     timestamptz not null default now()
);

-- Extend the outbox category allowlist for phenology stage products, both for
-- new installs (constraint above) and for existing installs that predate it.
do $$
declare
  def text;
begin
  select pg_get_constraintdef(c.oid) into def
  from pg_constraint c
  join pg_class t on t.oid = c.conrelid
  where t.relname = 'outbox_events' and c.contype = 'c'
    and pg_get_constraintdef(c.oid) like '%aggregate_metric%'
  limit 1;
  if def is not null and position('phenology_stage' in def) = 0 then
    alter table outbox_events drop constraint outbox_events_category_check;
    alter table outbox_events add constraint outbox_events_category_check
      check (category in ('raw_trajectory','operation_summary','device_alarm',
                          'aggregate_metric','phenology_stage'));
  end if;
end $$;
