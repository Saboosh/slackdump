#!/bin/bash
#
# Slack Export Automation Script
# Exports new Slack messages since the last successful run, up to the current time.
# Uses a checkpoint file (.last_export_time) to track state and avoid duplicates.
#
# Usage: ./run_slack_export.sh [hours_back]
#   hours_back: Optional. Fallback lookback window in hours if no checkpoint exists (default: 24)
#
# Output: slack_content_YYYYMMDD_HHMM.md stamped with the current run time
#
# State: .last_export_time stores the end timestamp of the last successful run.
#   - If the file exists, START_TIME is read from it (no overlap, no gaps).
#   - If missing (first run, or manual reset), falls back to hours_back.
#   - Only updated AFTER the full pipeline succeeds (export + process).
#   - To force a re-export: delete .last_export_time and run with desired hours_back.
#
# Requirements:
#   - slackdump installed and configured
#   - secrets.txt with SLACK_TOKEN environment variable (loaded via -load-env)
#   - channels.txt with list of channels to exclude from export
#   - process_export.py for converting export to markdown
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Fallback lookback window if no checkpoint exists (default: 24 hours)
HOURS_BACK="${1:-24}"

CHECKPOINT_FILE=".last_export_time"

# IMPORTANT: slackdump interprets -time-from/-time-to as UTC (no timezone in
# its parse format "2006-01-02T15:04:05"), so all timestamps must be in UTC.

# Determine START_TIME: from checkpoint if available, otherwise fallback
if [[ -f "$CHECKPOINT_FILE" ]]; then
    START_TIME=$(cat "$CHECKPOINT_FILE")
    echo "Resuming from checkpoint: ${START_TIME}"
else
    # No checkpoint — fall back to HOURS_BACK
    if [[ "$OSTYPE" == "darwin"* ]]; then
        START_TIME=$(date -u -v-${HOURS_BACK}H +%Y-%m-%dT%H:%M:%S)
    else
        START_TIME=$(date -u -d "${HOURS_BACK} hours ago" +%Y-%m-%dT%H:%M:%S)
    fi
    echo "No checkpoint found. Falling back to last ${HOURS_BACK} hours."
fi

# END_TIME is always now (UTC)
END_TIME=$(date -u +%Y-%m-%dT%H:%M:%S)
FILE_DATE=$(date +%Y%m%d_%H%M)

# Output filename (datetime-stamped so multiple runs per day don't collide)
OUTPUT_FILE="slack_content_${FILE_DATE}.md"

echo "=== Slack Export: ${START_TIME} to ${END_TIME} ==="
echo "Output will be written to: $OUTPUT_FILE"

# Clean up any previous temporary files
rm -rf export.zip extracted

# Run slackdump export
SLACK_START=$(date +%s)
echo "Running slackdump export..."
"${SCRIPT_DIR}/slackdump" export \
    -workspace corticoai \
    -load-env \
    -files=false \
    -time-from "${START_TIME}" \
    -time-to "${END_TIME}" \
    -o export.zip \
    @channels.txt
SLACK_END=$(date +%s)
SLACK_DURATION=$(( SLACK_END - SLACK_START ))
echo "Slackdump completed in ${SLACK_DURATION}s"

# Check if export was created
if [[ ! -f "export.zip" ]]; then
    echo "Error: export.zip was not created"
    exit 1
fi

EXPORT_SIZE=$(du -h export.zip | cut -f1)
echo "Export archive: ${EXPORT_SIZE}"

# Extract the export
echo "Extracting export..."
unzip -q export.zip -d extracted

# Process the export into markdown
PROC_START=$(date +%s)
echo "Processing export into markdown..."
python3 process_export.py extracted "$OUTPUT_FILE"
PROC_END=$(date +%s)
PROC_DURATION=$(( PROC_END - PROC_START ))
echo "Processing completed in ${PROC_DURATION}s"

# Check if processing found any messages — keep extracted dir for debugging if not
MSG_COUNT=$(python3 -c "
import re, sys
with open('$OUTPUT_FILE') as f:
    m = re.search(r'Messages: (\d+)', f.read())
    print(m.group(1) if m else '0')
")

if [[ "$MSG_COUNT" == "0" ]]; then
    echo "WARNING: 0 messages processed. Keeping 'extracted/' directory for debugging."
    echo "  Inspect with: ls -R extracted/ | head -50"
    rm -f export.zip
else
    # Clean up temporary files
    rm -rf export.zip extracted
fi

# --- Checkpoint update: ONLY after full pipeline success ---
echo "${END_TIME}" > "$CHECKPOINT_FILE"
echo "Checkpoint updated to: ${END_TIME}"

# --- Track channel activity over time ---
if [[ -f "channel_activity.json" ]]; then
    echo ""
    echo "Updating channel activity history..."
    python3 "${SCRIPT_DIR}/track_channel_activity.py"
fi

echo "=== Export complete: $OUTPUT_FILE (${START_TIME} to ${END_TIME}) ==="
