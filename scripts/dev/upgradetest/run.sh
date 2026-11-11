#!/usr/bin/env bash
# Device DB upgrade/rollback test (Phase 12). Reproducible from a clean tree:
#
#   scripts/dev/upgradetest/run.sh [work-dir]
#
#   1. builds v1tool (v1tool.go.in) against the device store exactly as at
#      commit 6286b80 (schema v1, migration 0001 only) in a git worktree, and
#      creates a v1 database with sessions and outbox events (two of them
#      retried once, so they carry v1-format timestamps);
#   2. opens it with the current device agent (migrates to the latest schema,
#      writes backups/pre-migration-v1-*.db first), runs one text session;
#      checks every original row is byte-identical and the DB passes
#      quick_check;
#   3. checks the v1 store refuses the newer database;
#   4. runs the rollback procedure from docs/LONG_TERM_RELIABILITY.md §7a
#      (quarantine the live DB, restore the pre-migration copy, salvage
#      unacknowledged events and sessions recorded after the upgrade) and
#      checks the v1 store opens it with every event present;
#   5. rolls forward again with the current binary.
# Uses port 127.0.0.1:8760 and needs sqlite3, jq, curl. Prints PASS/FAIL lines.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"; cd "$ROOT"
S="${1:-$(mktemp -d)}"; mkdir -p "$S"
WT="$S/store-6286b80"
if [[ ! -d "$WT" ]]; then git worktree add --detach "$WT" 6286b80 >/dev/null; fi
mkdir -p "$WT/services/device-agent/cmd/v1tool"
cp scripts/dev/upgradetest/v1tool.go.in "$WT/services/device-agent/cmd/v1tool/main.go"
(cd "$WT" && go build -o "$S/v1tool" ./services/device-agent/cmd/v1tool) || { echo "FAIL build v1tool"; exit 1; }
go build -o "$S/cur-agent" ./services/device-agent/cmd/device-agent || { echo "FAIL build agent"; exit 1; }
trap 'git worktree remove --force "$WT" >/dev/null 2>&1' EXIT
D=$S/upg; rm -rf "$D"; mkdir -p "$D"
q() { sqlite3 "$1" "$2"; }  # agent is stopped whenever q runs
echo "== 1. create v1 database with the 6286b80 store"
$S/v1tool create $D/device.db 5
q $D/device.db "PRAGMA user_version" | sed 's/^/user_version=/'
q $D/device.db "SELECT id,type,payload_sha256,state,attempts,payload FROM outbox_events ORDER BY id" > $S/pre_outbox.txt
q $D/device.db "SELECT * FROM sessions ORDER BY id" > $S/pre_sessions.txt
echo "outbox rows: $(wc -l < $S/pre_outbox.txt); sessions: $(wc -l < $S/pre_sessions.txt); v1 next_attempt_at widths: $(q $D/device.db "SELECT group_concat(DISTINCT length(next_attempt_at)) FROM outbox_events")"
echo "== 2. open with the current agent binary"
jq --arg dd "$D" '.data_dir=$dd | .listen="127.0.0.1:8760" | .speech.stt_engine="none" | .speech.tts_enabled=false | .llm.enabled=false | .documents.enabled=false | .sync.base_url="" | .sync.server_keys={}' config/device.dev.json > $D/config.json
$S/cur-agent -config $D/config.json > $D/agent.log 2>&1 & P=$!
for i in $(seq 1 40); do curl -s -o /dev/null -m 1 http://127.0.0.1:8760/api/v1/status && break; sleep 0.25; done
A=http://127.0.0.1:8760/api/v1; post() { curl -s -H 'X-JS-Kiosk: 1' -X POST "$@"; }
SID=$(post -H 'Content-Type: application/json' -d '{"language":"hi"}' $A/session/start | jq -r .session.id)
post -H 'Content-Type: application/json' -d '{"text":"मुझे लोन चाहिए"}' $A/session/$SID/text | jq -c '{turn:.reply.kind}'
post $A/session/$SID/end | jq -c '{end:.session.activity}'
kill -TERM $P; wait $P; echo "agent exit=$?"
grep -o '"msg":"[^"]*migrat[^"]*"' $D/agent.log | head -3
echo "user_version=$(q $D/device.db 'PRAGMA user_version')"
ls $D/backups/
q $D/device.db "SELECT id,type,payload_sha256,state,attempts,payload FROM outbox_events WHERE id IN (SELECT id FROM outbox_events) ORDER BY id" | grep -F -f <(cut -d'|' -f1 $S/pre_outbox.txt) > $S/post_outbox_orig.txt
[[ -s $S/pre_outbox.txt ]] && diff -q $S/pre_outbox.txt $S/post_outbox_orig.txt && echo "PASS original outbox rows identical (id, type, hash, state, attempts, payload)" || echo "FAIL original outbox rows differ"
diff -q $S/pre_sessions.txt <(q $D/device.db "SELECT * FROM sessions WHERE id IN ($(cut -d'|' -f1 $S/pre_sessions.txt | sed "s/.*/'&'/" | paste -sd, -)) ORDER BY id") && echo "PASS original session rows identical" || echo "FAIL session rows differ"
C=$(q $D/device.db 'SELECT count(*) FROM outbox_events'); N=$(q $D/device.db 'SELECT n FROM outbox_counter'); PRE=$(wc -l < $S/pre_outbox.txt)
[[ "$C" == "$N" && $C -gt $PRE ]] && echo "PASS outbox_counter $N = rows $C ($((C-PRE)) new events after upgrade)" || echo "FAIL counter $N rows $C pre $PRE"
echo "next_attempt_at widths after: $(q $D/device.db "SELECT group_concat(DISTINCT length(next_attempt_at)) FROM outbox_events"); claimed_at column: $(q $D/device.db "SELECT count(*) FROM pragma_table_info('outbox_events') WHERE name='claimed_at'")"
echo "quick_check: $(q $D/device.db 'PRAGMA quick_check')"
echo "== 3. older (v1) binary on the v2 database"
$S/v1tool open $D/device.db; rc=$?; [[ $rc == 3 ]] && echo "PASS v1 store refuses the v2 database" || echo "FAIL v1 store exit $rc"
echo "== 4. rollback procedure"
RB=$D/rollback-$(date -u +%Y%m%dT%H%M%SZ); mkdir -p $RB
mv $D/device.db $RB/; mv $D/device.db-wal $RB/ 2>/dev/null; mv $D/device.db-shm $RB/ 2>/dev/null
cp "$(ls $D/backups/pre-migration-v1-*.db | tail -1)" $D/device.db
echo "restored user_version=$(q $D/device.db 'PRAGMA user_version')"
sqlite3 $D/device.db <<SQL
ATTACH '$RB/device.db' AS n;
BEGIN;
INSERT OR IGNORE INTO sessions SELECT id, started_at, ended_at, language, end_reason, turns FROM n.sessions;
-- A session open at backup time and closed after the upgrade: take the
-- newer end, so rolling forward again does not close it a second time.
UPDATE sessions SET (ended_at, end_reason, turns) = (SELECT s.ended_at, s.end_reason, max(sessions.turns, s.turns) FROM n.sessions s WHERE s.id = sessions.id)
  WHERE ended_at IS NULL AND id IN (SELECT id FROM n.sessions WHERE ended_at IS NOT NULL);
INSERT OR IGNORE INTO outbox_events (id, type, schema_version, payload, payload_sha256, created_at, state, attempts, next_attempt_at)
  SELECT id, type, schema_version, payload, payload_sha256, created_at, 'PENDING', 0, strftime('%Y-%m-%dT%H:%M:%fZ','now')
  FROM n.outbox_events WHERE state NOT IN ('ACKNOWLEDGED','DELETE_ELIGIBLE');
COMMIT;
DETACH n;
SQL
echo "salvage exit=$?"
$S/v1tool open $D/device.db && echo "PASS v1 store opens the restored database" || echo "FAIL v1 store cannot open the restored database"
diff <(q $RB/device.db "SELECT id,payload_sha256,payload FROM outbox_events ORDER BY id") <(q $D/device.db "SELECT id,payload_sha256,payload FROM outbox_events ORDER BY id") && echo "PASS every event (pre-upgrade and post-upgrade) present after rollback, payloads identical" || echo "FAIL events differ after rollback"
echo "quick_check: $(q $D/device.db 'PRAGMA quick_check')"
echo "== 5. roll forward again with the current binary"
$S/cur-agent -config $D/config.json > $D/agent2.log 2>&1 & P=$!
for i in $(seq 1 40); do curl -s -o /dev/null -m 1 http://127.0.0.1:8760/api/v1/status && break; sleep 0.25; done
curl -s $A/status | jq -c '{overall}'; kill -TERM $P; wait $P
echo "user_version=$(q $D/device.db 'PRAGMA user_version') rows=$(q $D/device.db 'SELECT count(*) FROM outbox_events') counter=$(q $D/device.db 'SELECT n FROM outbox_counter')"; ls $D/backups/
