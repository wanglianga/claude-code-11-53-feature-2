#!/usr/bin/env python3
"""Generate phenology stage-alignment samples under samples/phenology/.

One season rule version binds every region. Northern-hemisphere regions sow in
April 2026; the southern hemisphere region (BR-MT) sows in October 2026, so a
single calendar week does NOT correspond to one growth stage. The samples are
built to exercise:

  * same days-after-sowing but different temperature -> different GDD stage
  * different variety maturity under the same day count
  * a reseeded plot that must be segmented, not pooled
  * a weather-station gap that widens the evidence range
  * a station link that is too far / too high (must go pending)
  * a multi-crop plot (must go pending)
  * a variety with no day thresholds (calendar fallback)
  * a late-corrected sowing fact and a weather revision (immutability proofs)

Usage: python3 scripts/gen_phenology_samples.py
"""
import json
import os
from datetime import date, timedelta

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "samples", "phenology")
RULE_ID = "S2026-MAIZE-v1"
SEASON_ID = "2026"
RULE_VERSION = "2026.1"


def d(iso):
    return date.fromisoformat(iso)


def iso(x):
    return x.isoformat()


def rule():
    return {
        "rule_id": RULE_ID,
        "season_id": SEASON_ID,
        "rule_version": RULE_VERSION,
        "crop": "maize",
        "hemisphere": "north",
        "das_tolerance_days": 3,
        "weather_gap_pending_days": 12,
        "min_weather_coverage": 0.8,
        "station_max_distance_km": 40,
        "station_max_elevation_delta_m": 120,
        "missing_day_upper_fraction": 0.6,
        "published_at": "2026-03-01T00:00:00Z",
        "notes": [
            "Season rule version 2026.1 binds sowing facts, variety catalog, weather evidence, derivation and group comparison.",
            "Day thresholds are cumulative days after sowing; GDD thresholds are cumulative growing degree days above the variety base temperature.",
        ],
    }


def variety(code, maturity, das, gdd=None):
    return {
        "variety_code": code,
        "crop": "maize",
        "maturity_group": maturity,
        "das_thresholds": das,
        "gdd": gdd,
    }


def gdd_params(base, emergence, vegetative, harvest_prep):
    return {"base_temp_c": base,
            "thresholds": {"emergence": emergence, "vegetative": vegetative, "harvest_prep": harvest_prep}}


def plot(pid, area, note=""):
    return {"plot_id": pid, "crop": "maize", "declared_area_ha": area, "note": note}


def fact(fid, plot_id, seg, sown, area, variety_code, event="initial", rev=1,
         evidence="seed-ticket", recorded="2026-04-02T08:00:00Z", status="active", crop="maize"):
    return {
        "fact_id": fid, "plot_id": plot_id, "segment_no": seg, "crop": crop,
        "variety_code": variety_code, "sown_on": iso(sown), "area_ha": area,
        "event_type": event, "evidence_ref": evidence, "recorded_at": recorded,
        "revision": rev, "status": status,
    }


def obs(station, day, tmin, tmax, src="region-weather-feed", rev=1, recorded="2026-04-30T00:00:00Z"):
    return {"station_id": station, "obs_date": iso(day), "tmin_c": tmin, "tmax_c": tmax,
            "source": src, "revision": rev, "recorded_at": recorded}


def daily(station, start, count, tmin, tmax, skip=(), src="region-weather-feed"):
    out = []
    for i in range(count):
        day = start + timedelta(days=i)
        if i in skip:
            continue
        out.append(obs(station, day, tmin, tmax, src=src))
    return out


def write(rel, payload):
    path = os.path.join(OUT, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
        fh.write("\n")
    print("wrote", os.path.relpath(path, ROOT))


def base_stations(prefix, tmin, tmax, elev):
    return [{"station_id": f"{prefix}-WX1", "label": f"{prefix} primary", "elevation_m": elev}]


def de_bb():
    # DE-BB: cool spring. Varieties VEARLY (short) and VLATE (long). Plot BB-F-02
    # cannot be attributed: an unknown variety plus a multi-crop segment.
    st = "DE-WX1"
    ev = {
        "batch_id": "phenology-de-bb-v1",
        "rule": rule(),
        "varieties": [
            variety("VEARLY", "early", {"emergence": 8, "vegetative": 40, "harvest_prep": 120},
                    gdd_params(6.0, 100, 620, 1400)),
            variety("VLATE", "late", {"emergence": 10, "vegetative": 60, "harvest_prep": 150},
                    gdd_params(6.0, 120, 760, 1600)),
            # No day thresholds and no GDD: forces the calendar fallback for DAS
            # and pending (gdd_params_not_declared) for GDD.
            variety("VUNKNOWN-DAYS", "regional", {}),
        ],
        "plots": [
            plot("BB-F-01", 120.0), plot("BB-F-02", 80.0, "split crop"),
            plot("BB-F-03", 100.0), plot("BB-F-04", 60.0),
        ],
        "sowing_facts": [
            fact("BB-F-01#1", "BB-F-01", 1, d("2026-04-10"), 120.0, "VEARLY"),
            fact("BB-F-02#1", "BB-F-02", 1, d("2026-04-12"), 50.0, "VLATE"),
            # A second, different crop on the same plot => multi_crop_plot.
            fact("BB-F-02#2", "BB-F-02", 2, d("2026-04-20"), 30.0, "VLATE", crop="wheat"),
            fact("BB-F-03#1", "BB-F-03", 1, d("2026-04-08"), 100.0, "VLATE"),
            # Unknown variety code entirely.
            fact("BB-F-04#1", "BB-F-04", 1, d("2026-04-15"), 60.0, "V-UNDECLARED"),
        ],
        "stage_calendars": [
            {"calendar_id": "CAL-MAIZE-REGIONAL", "crop": "maize", "variety_code": "VUNKNOWN-DAYS",
             "stage": "emergence", "start_day": 9, "end_day": 14, "source": "agronomy-bulletin-2026-04",
             "published_at": "2026-04-01T00:00:00Z"},
            {"calendar_id": "CAL-MAIZE-REGIONAL-V", "crop": "maize", "variety_code": "VUNKNOWN-DAYS",
             "stage": "vegetative", "start_day": 45, "end_day": 90, "source": "agronomy-bulletin-2026-04",
             "published_at": "2026-04-01T00:00:00Z"},
        ],
        "stations": base_stations("DE", 6, 16, 40),
        "station_links": [
            {"plot_id": "BB-F-01", "station_id": st, "distance_km": 12.0, "elevation_delta_m": 8.0},
            {"plot_id": "BB-F-02", "station_id": st, "distance_km": 18.0, "elevation_delta_m": 12.0},
            {"plot_id": "BB-F-03", "station_id": st, "distance_km": 22.0, "elevation_delta_m": 15.0},
            {"plot_id": "BB-F-04", "station_id": st, "distance_km": 30.0, "elevation_delta_m": 20.0},
        ],
        "temperature_observations": daily(st, d("2026-04-08"), 60, 6, 16),
        "stage_observations": [],
    }
    write("de-bb/evidence.json", ev)


def us_ia():
    # US-IA: warmer spring than DE-BB. Same 30 days after sowing from 2026-04-15
    # lands at a further GDD stage. IA-F-22 has a station link that is too far.
    st = "US-WX1"
    ev = {
        "batch_id": "phenology-us-ia-v1",
        "rule": rule(),
        "varieties": [
            # Same declared day thresholds as DE-BB's VEARLY so the DAS route
            # yields the same stage at the same days-after-sowing; the warmer
            # GDD parameters let the same 30 DAS reach a further GDD stage.
            variety("VEARLY", "early", {"emergence": 8, "vegetative": 40, "harvest_prep": 120},
                    gdd_params(10.0, 120, 250, 900)),
            variety("VLATE", "late", {"emergence": 10, "vegetative": 60, "harvest_prep": 150},
                    gdd_params(10.0, 140, 820, 1750)),
        ],
        "plots": [plot("IA-F-21", 130.0), plot("IA-F-22", 90.0), plot("IA-F-23", 110.0)],
        "sowing_facts": [
            fact("IA-F-21#1", "IA-F-21", 1, d("2026-04-15"), 130.0, "VEARLY"),
            fact("IA-F-22#1", "IA-F-22", 1, d("2026-04-15"), 90.0, "VEARLY"),
            fact("IA-F-23#1", "IA-F-23", 1, d("2026-04-18"), 110.0, "VLATE"),
        ],
        "stage_calendars": [],
        "stations": [{"station_id": st, "label": "US primary", "elevation_m": 300}],
        "station_links": [
            {"plot_id": "IA-F-21", "station_id": st, "distance_km": 10.0, "elevation_delta_m": 5.0},
            # Too far AND too high: must be pending, never borrow a distant station.
            {"plot_id": "IA-F-22", "station_id": st, "distance_km": 95.0, "elevation_delta_m": 260.0},
            {"plot_id": "IA-F-23", "station_id": st, "distance_km": 14.0, "elevation_delta_m": 6.0},
        ],
        # Warmer than DE-BB: tmin 12, tmax 26.
        "temperature_observations": daily(st, d("2026-04-15"), 55, 12, 26),
        "stage_observations": [],
    }
    write("us-ia/evidence.json", ev)


def br_mt():
    # BR-MT (southern hemisphere): sown in October 2026. A weather gap in the
    # middle of the window widens the GDD evidence range. One plot reseeded.
    st = "BR-WX1"
    ev = {
        "batch_id": "phenology-br-mt-v1",
        "rule": {**rule(), "hemisphere": "south",
                 "notes": ["Southern-hemisphere season: October sowing. Compared to the north by local equivalent date, not calendar week."]},
        "varieties": [
            variety("VBR-EARLY", "early", {"emergence": 7, "vegetative": 38, "harvest_prep": 118},
                    gdd_params(12.0, 140, 400, 1550)),
        ],
        "plots": [plot("MT-F-11", 140.0), plot("MT-F-12", 100.0, "reseeded"), plot("MT-F-13", 90.0)],
        "sowing_facts": [
            fact("MT-F-11#1", "MT-F-11", 1, d("2026-10-05"), 140.0, "VBR-EARLY",
                 recorded="2026-10-06T08:00:00Z"),
            # Plot 12 reseeded: original sowing failed, reseed is a second segment.
            fact("MT-F-12#1", "MT-F-12", 1, d("2026-10-02"), 100.0, "VBR-EARLY",
                 recorded="2026-10-03T08:00:00Z"),
            fact("MT-F-12#2", "MT-F-12", 2, d("2026-10-14"), 100.0, "VBR-EARLY", event="reseed",
                 evidence="reseed-ticket", recorded="2026-10-15T08:00:00Z"),
            fact("MT-F-13#1", "MT-F-13", 1, d("2026-10-08"), 90.0, "VBR-EARLY",
                 recorded="2026-10-09T08:00:00Z"),
        ],
        "stage_calendars": [],
        "stations": [{"station_id": st, "label": "BR primary", "elevation_m": 380}],
        "station_links": [
            {"plot_id": "MT-F-11", "station_id": st, "distance_km": 9.0, "elevation_delta_m": 10.0},
            {"plot_id": "MT-F-12", "station_id": st, "distance_km": 12.0, "elevation_delta_m": 14.0},
            {"plot_id": "MT-F-13", "station_id": st, "distance_km": 15.0, "elevation_delta_m": 18.0},
        ],
        # Hot early season with a 12-day gap in the middle of the window
        # (2026-10-18..2026-10-29). The gap-widened upper bound crosses the
        # vegetative threshold while the lower bound does not, so the two
        # earliest sowings are reported pending with a wide evidence range. The
        # later reseeded segment stays below both bounds and is classified.
        "temperature_observations": daily(st, d("2026-10-02"), 40, 22, 36, skip=tuple(range(16, 28))),
        "stage_observations": [],
    }
    write("br-mt/evidence.json", ev)


def fr_nor():
    # FR-NOR: one plot uses the calendar fallback (variety declares no thresholds).
    st = "FR-WX1"
    ev = {
        "batch_id": "phenology-fr-nor-v1",
        "rule": rule(),
        "varieties": [
            variety("VEARLY", "early", {"emergence": 9, "vegetative": 42, "harvest_prep": 125},
                    gdd_params(6.0, 110, 640, 1450)),
            variety("VCALENDAR", "regional", {}, None),
        ],
        "plots": [plot("NOR-F-31", 70.0), plot("NOR-F-32", 55.0)],
        "sowing_facts": [
            fact("NOR-F-31#1", "NOR-F-31", 1, d("2026-04-14"), 70.0, "VEARLY"),
            fact("NOR-F-32#1", "NOR-F-32", 1, d("2026-04-16"), 55.0, "VCALENDAR"),
        ],
        "stage_calendars": [
            {"calendar_id": "CAL-NOR-CALENDAR", "crop": "maize", "variety_code": "VCALENDAR",
             "stage": "emergence", "start_day": 10, "end_day": 15, "source": "agronomy-bulletin-2026-04",
             "published_at": "2026-04-01T00:00:00Z"},
            {"calendar_id": "CAL-NOR-CALENDAR-V", "crop": "maize", "variety_code": "VCALENDAR",
             "stage": "vegetative", "start_day": 48, "end_day": 95, "source": "agronomy-bulletin-2026-04",
             "published_at": "2026-04-01T00:00:00Z"},
        ],
        "stations": [{"station_id": st, "label": "FR primary", "elevation_m": 60}],
        "station_links": [
            {"plot_id": "NOR-F-31", "station_id": st, "distance_km": 11.0, "elevation_delta_m": 5.0},
            {"plot_id": "NOR-F-32", "station_id": st, "distance_km": 16.0, "elevation_delta_m": 9.0},
        ],
        "temperature_observations": daily(st, d("2026-04-14"), 55, 8, 18),
        # A scouting note contradicts the derived stage for NOR-F-31 (which by
        # day thresholds is only at emergence): the plot must be left pending
        # rather than trusting either source blindly.
        "stage_observations": [
            {"observation_id": "NOR-F-31-scout-1", "plot_id": "NOR-F-31", "segment_no": 1,
             "stage": "vegetative", "observed_on": "2026-05-15", "source": "field-scout"},
        ],
    }
    write("fr-nor/evidence.json", ev)


def corrections():
    # A late correction of a sowing date (higher revision) and a weather
    # revision. Applying these must not change an existing run; they only
    # feed the next run.
    ev = {
        "batch_id": "phenology-de-bb-correction-v2",
        "rule": rule(),
        "varieties": [],
        "plots": [],
        "sowing_facts": [
            # BB-F-03's sowing is corrected from 2026-04-08 to 2026-04-18.
            fact("BB-F-03#1", "BB-F-03", 1, d("2026-04-18"), 100.0, "VLATE", rev=2,
                 evidence="corrected-planting-log", recorded="2026-05-20T09:00:00Z"),
        ],
        "stage_calendars": [],
        "stations": [],
        "station_links": [],
        "temperature_observations": [
            # A corrected reading for 2026-04-20 (was 6/16, now 12/24).
            obs("DE-WX1", d("2026-04-20"), 12, 24, src="corrected-feed", rev=2,
                recorded="2026-05-20T09:05:00Z"),
        ],
        "stage_observations": [],
    }
    write("de-bb/correction-v2.json", ev)


def main():
    de_bb()
    us_ia()
    br_mt()
    fr_nor()
    corrections()

    # Point the existing approval sample's scope at the phenology-safe subset
    # (all four regions authorize area_ha) by writing a dedicated approval.
    approval = {
        "purpose": "Cross-hemisphere maize stage alignment review",
        "recipient": "group-agronomy@example.org",
        "scope_regions": ["BR-MT", "DE-BB", "FR-NOR", "US-IA"],
        "scope_metrics": ["area_ha", "operation_count", "completion_rate"],
        "valid_until": "2026-12-31T23:59:59Z",
    }
    write("approvals/phenology-wide.json", approval)


if __name__ == "__main__":
    main()
