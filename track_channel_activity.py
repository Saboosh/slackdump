#!/usr/bin/env python3
"""
Track channel activity across slackdump export runs.

Reads channel_activity.json (produced by each export run), accumulates
history in channel_history.json, and prints a summary report showing
which channels are consistently empty and safe to exclude.

Usage:
    python3 track_channel_activity.py [--history channel_history.json] [--min-runs N]

Options:
    --history FILE   Path to the persistent history file (default: channel_history.json)
    --min-runs N     Minimum runs before a channel is reported as "safe to exclude" (default: 3)
"""

import argparse
import json
import os
import sys
from datetime import datetime


ACTIVITY_FILE = "channel_activity.json"
DEFAULT_HISTORY = "channel_history.json"


def load_json(path):
    if not os.path.exists(path):
        return None
    with open(path) as f:
        return json.load(f)


def save_json(path, data):
    with open(path, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")


def ingest_run(history, activity):
    """Add a single run's activity data to the history."""
    timestamp = activity["timestamp"]

    # Skip if this exact timestamp was already ingested.
    if any(r["timestamp"] == timestamp for r in history["runs"]):
        return False

    channels = {}
    for ch in activity["channels"]:
        channels[ch["id"]] = {
            "name": ch.get("name", ""),
            "messages": ch["messages"],
        }

    history["runs"].append({
        "timestamp": timestamp,
        "channels": channels,
    })
    return True


def generate_report(history, min_runs):
    """Generate a summary report from accumulated history."""
    runs = history["runs"]
    total_runs = len(runs)

    if total_runs == 0:
        print("No runs recorded yet.")
        return

    # Collect per-channel stats across all runs.
    # channel_id -> {name, empty_runs, total_runs_seen, last_active, total_messages}
    channel_stats = {}
    for run in runs:
        ts = run["timestamp"]
        for ch_id, info in run["channels"].items():
            if ch_id not in channel_stats:
                channel_stats[ch_id] = {
                    "name": info["name"],
                    "empty_runs": 0,
                    "active_runs": 0,
                    "total_runs_seen": 0,
                    "last_active": None,
                    "total_messages": 0,
                }
            stats = channel_stats[ch_id]
            stats["total_runs_seen"] += 1
            stats["total_messages"] += info["messages"]
            # Keep name up to date (it might have been empty in earlier runs).
            if info["name"]:
                stats["name"] = info["name"]
            if info["messages"] == 0:
                stats["empty_runs"] += 1
            else:
                stats["active_runs"] += 1
                stats["last_active"] = ts

    # Categorize channels.
    always_empty = []  # empty in every run they appeared in
    mostly_empty = []  # empty in >50% of runs
    active = []        # active in >50% of runs

    for ch_id, stats in channel_stats.items():
        seen = stats["total_runs_seen"]
        entry = {
            "id": ch_id,
            "name": stats["name"],
            "empty_runs": stats["empty_runs"],
            "total_runs_seen": seen,
            "last_active": stats["last_active"],
            "total_messages": stats["total_messages"],
        }
        if stats["empty_runs"] == seen:
            always_empty.append(entry)
        elif stats["empty_runs"] > seen / 2:
            mostly_empty.append(entry)
        else:
            active.append(entry)

    always_empty.sort(key=lambda x: x["name"])
    mostly_empty.sort(key=lambda x: -x["empty_runs"])
    active.sort(key=lambda x: -x["total_messages"])

    # Date range.
    first_run = runs[0]["timestamp"][:10]
    last_run = runs[-1]["timestamp"][:10]

    print("=" * 70)
    print(f"  CHANNEL ACTIVITY REPORT")
    print(f"  {total_runs} runs from {first_run} to {last_run}")
    print(f"  {len(channel_stats)} total channels tracked")
    print("=" * 70)

    # Always empty.
    print(f"\n  ALWAYS EMPTY ({len(always_empty)} channels)")
    if total_runs >= min_runs:
        print(f"  Safe to exclude (empty in all {total_runs} runs):")
    else:
        print(f"  Need {min_runs - total_runs} more run(s) before recommending exclusion.")
    print(f"  {'Channel':<45} {'Empty Runs':>10}")
    print(f"  {'-'*45} {'-'*10}")
    for ch in always_empty:
        label = f"#{ch['name']}" if ch["name"] else ch["id"]
        print(f"  {label:<45} {ch['empty_runs']:>5}/{ch['total_runs_seen']}")

    # Mostly empty.
    if mostly_empty:
        print(f"\n  MOSTLY EMPTY ({len(mostly_empty)} channels)")
        print(f"  {'Channel':<45} {'Empty Runs':>10} {'Last Active':>12}")
        print(f"  {'-'*45} {'-'*10} {'-'*12}")
        for ch in mostly_empty:
            label = f"#{ch['name']}" if ch["name"] else ch["id"]
            last = ch["last_active"][:10] if ch["last_active"] else "never"
            print(f"  {label:<45} {ch['empty_runs']:>5}/{ch['total_runs_seen']} {last:>12}")

    # Active.
    if active:
        print(f"\n  ACTIVE ({len(active)} channels)")
        print(f"  {'Channel':<45} {'Total Msgs':>10} {'Active Runs':>12}")
        print(f"  {'-'*45} {'-'*10} {'-'*12}")
        for ch in active[:30]:  # Show top 30 by message count.
            label = f"#{ch['name']}" if ch["name"] else ch["id"]
            active_runs = ch["total_runs_seen"] - ch["empty_runs"]
            print(f"  {label:<45} {ch['total_messages']:>10} {active_runs:>5}/{ch['total_runs_seen']}")
        if len(active) > 30:
            print(f"  ... and {len(active) - 30} more active channels")

    print()

    # Write exclusion candidates to file if enough runs.
    if total_runs >= min_runs and always_empty:
        exclude_file = "exclude_candidates.txt"
        with open(exclude_file, "w") as f:
            f.write(f"# Channels empty in all {total_runs} runs ({first_run} to {last_run})\n")
            f.write(f"# Generated by track_channel_activity.py\n")
            for ch in always_empty:
                if ch["name"]:
                    f.write(f"^{ch['id']}  # {ch['name']}\n")
                else:
                    f.write(f"^{ch['id']}\n")
        print(f"  Wrote {len(always_empty)} exclusion candidates to {exclude_file}")
        print()


def main():
    parser = argparse.ArgumentParser(description="Track channel activity across export runs")
    parser.add_argument("--history", default=DEFAULT_HISTORY, help="Path to history file")
    parser.add_argument("--min-runs", type=int, default=3, help="Minimum runs before recommending exclusion")
    args = parser.parse_args()

    # Load or initialize history.
    history = load_json(args.history)
    if history is None:
        history = {"runs": []}

    # Ingest latest run if activity file exists.
    activity = load_json(ACTIVITY_FILE)
    if activity is None:
        if not history["runs"]:
            print(f"No {ACTIVITY_FILE} found and no history. Run an export first.", file=sys.stderr)
            sys.exit(1)
        print(f"No new {ACTIVITY_FILE} found. Showing report from existing history.\n")
    else:
        if ingest_run(history, activity):
            save_json(args.history, history)
            print(f"Ingested run from {activity['timestamp']}\n")
        else:
            print(f"Run {activity['timestamp']} already in history, skipping.\n")

    generate_report(history, args.min_runs)


if __name__ == "__main__":
    main()
