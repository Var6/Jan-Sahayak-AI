"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import { type FormEvent, Suspense, useState } from "react";
import { useAuth } from "@/components/Auth";
import { Can, Shell } from "@/components/Shell";
import { ConnectivityBadge, ErrorBox, HealthBadge } from "@/components/ui";
import { errorMessage } from "@/lib/api";
import { connectivity } from "@/lib/devices";
import { fmtBytes, fmtTime } from "@/lib/format";
import type { Device, DeviceDetail } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

function Revoke({ device, onDone }: { device: Device; onDone: () => void }) {
  const { api } = useAuth();
  const [reason, setReason] = useState("");
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (device.status === "revoked") return null;
  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.revokeDevice(device.id, reason.trim());
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  const ok = reason.trim().length >= 1 && [...reason.trim()].length <= 200 && confirm;
  return (
    <details className="panel danger">
      <summary>Revoke this device</summary>
      <form onSubmit={onSubmit} className="stack">
        <p>
          Revoking is permanent: the device can no longer sync or fetch bundles. Its stored events
          are kept. A replacement must register a new identity.
        </p>
        <label>
          Reason (required, max 200 characters)
          <input
            value={reason}
            maxLength={200}
            required
            onChange={(e) => setReason(e.target.value)}
          />
        </label>
        <label className="check">
          <input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />
          I understand this cannot be undone.
        </label>
        <ErrorBox error={error} />
        <button type="submit" className="danger" disabled={!ok || busy}>
          {busy ? "Revoking…" : "Revoke device"}
        </button>
      </form>
    </details>
  );
}

function Detail({ d, reload }: { d: DeviceDetail; reload: () => void }) {
  const hb = d.last_heartbeat;
  const now = Date.now();
  const conn = connectivity({ status: d.device.status, last_heartbeat: hb }, now);
  return (
    <>
      <section className="panel">
        <dl className="kv">
          <dt>Status</dt>
          <dd>
            <ConnectivityBadge c={conn} />{" "}
            {d.device.status === "revoked" &&
              `${fmtTime(d.device.revoked_at)} — ${d.device.revoked_reason ?? ""}`}
          </dd>
          <dt>District</dt>
          <dd>{d.device.district}</dd>
          <dt>Device id</dt>
          <dd>
            <code>{d.device.id}</code>
          </dd>
          <dt>Registered</dt>
          <dd>{fmtTime(d.device.registered_at)}</dd>
          <dt>Last seen</dt>
          <dd>{fmtTime(d.device.last_seen_at)}</dd>
          <dt>Events received</dt>
          <dd>
            {d.sync.events_total} total, {d.sync.events_24h} in the last 24 h; last at{" "}
            {fmtTime(d.sync.last_event_received_at)}
          </dd>
        </dl>
      </section>
      {hb ? (
        <section className="panel">
          <h2>Latest heartbeat</h2>
          <dl className="kv">
            <dt>Received</dt>
            <dd>
              {fmtTime(hb.received_at)} (device clock {fmtTime(hb.device_time)})
            </dd>
            <dt>Health</dt>
            <dd>
              <HealthBadge state={hb.overall_health} />
            </dd>
            <dt>Versions</dt>
            <dd>
              agent {hb.agent_version || "—"} · bundle {hb.bundle_version || "none"} ·{" "}
              {hb.os || "OS unknown"}
            </dd>
            <dt>Outbox</dt>
            <dd>
              {hb.outbox_pending} pending, {hb.outbox_uploading} uploading, {hb.outbox_failed}{" "}
              failed, {hb.outbox_conflict} conflict · {hb.outbox_total}/{hb.outbox_capacity} used
              {hb.outbox_oldest_pending_at &&
                ` · oldest pending ${fmtTime(hb.outbox_oldest_pending_at)}`}
            </dd>
            <dt>Sync</dt>
            <dd>
              last success {fmtTime(hb.sync_last_success_at)} · {hb.sync_consecutive_failures}{" "}
              consecutive failures
            </dd>
            <dt>Disk</dt>
            <dd>
              {fmtBytes(hb.disk_free_bytes)} free of {fmtBytes(hb.disk_total_bytes)}
            </dd>
            <dt>Memory</dt>
            <dd>
              {fmtBytes(hb.mem_available_bytes)} available of {fmtBytes(hb.mem_total_bytes)}
            </dd>
            <dt>CPU temperature</dt>
            <dd>
              {hb.cpu_temp_c !== undefined ? `${hb.cpu_temp_c.toFixed(1)} °C` : "—"}
              {hb.throttled ? " · throttled" : ""}
            </dd>
            <dt>Uptime</dt>
            <dd>
              {hb.uptime_seconds !== undefined ? `${Math.floor(hb.uptime_seconds / 3600)} h` : "—"}
            </dd>
          </dl>
          {hb.components && hb.components.length > 0 && (
            <div className="table-scroll">
              <table>
                <caption>Components</caption>
                <thead>
                  <tr>
                    <th scope="col">Component</th>
                    <th scope="col">State</th>
                    <th scope="col">Detail code</th>
                  </tr>
                </thead>
                <tbody>
                  {hb.components.map((c) => (
                    <tr key={c.name}>
                      <td>
                        {c.name}
                        {c.critical ? " (critical)" : ""}
                      </td>
                      <td>
                        <HealthBadge state={c.state} />
                      </td>
                      <td>
                        <code>{c.detail || "—"}</code>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ) : (
        <p className="panel">This device has not sent a heartbeat yet.</p>
      )}
      {d.recent_heartbeats.length > 0 && (
        <section className="panel">
          <h2>Heartbeat history (latest {d.recent_heartbeats.length})</h2>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">Received</th>
                  <th scope="col">Health</th>
                  <th scope="col">Agent</th>
                  <th scope="col" className="num">
                    Pending
                  </th>
                  <th scope="col" className="num">
                    Failed
                  </th>
                  <th scope="col">Disk free</th>
                  <th scope="col">Mem avail.</th>
                  <th scope="col" className="num">
                    °C
                  </th>
                </tr>
              </thead>
              <tbody>
                {d.recent_heartbeats.map((h) => (
                  <tr key={h.received_at}>
                    <td>{fmtTime(h.received_at)}</td>
                    <td>
                      <HealthBadge state={h.overall_health} />
                    </td>
                    <td>{h.agent_version}</td>
                    <td className="num">{h.outbox_pending}</td>
                    <td className="num">{h.outbox_failed + h.outbox_conflict}</td>
                    <td>{fmtBytes(h.disk_free_bytes)}</td>
                    <td>{fmtBytes(h.mem_available_bytes)}</td>
                    <td className="num">
                      {h.cpu_temp_c !== undefined ? h.cpu_temp_c.toFixed(0) : "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
      <Can cap="manageDevices">
        <Revoke device={d.device} onDone={reload} />
      </Can>
    </>
  );
}

function DeviceDetailView() {
  const { api } = useAuth();
  const id = useSearchParams().get("id") ?? "";
  const { data, error, loading, reload } = useLoad(
    (s) => (id ? api.device(id, s) : Promise.reject(new Error("No device id in the address."))),
    [api, id],
  );
  return (
    <>
      <p>
        <Link href="/devices/">← All devices</Link>
      </p>
      {data && <h2 className="subtitle">{data.device.name}</h2>}
      <ErrorBox error={error} />
      {loading && !data && <p className="muted">Loading…</p>}
      {data && <Detail d={data} reload={reload} />}
    </>
  );
}

export default function DeviceDetailPage() {
  return (
    <Shell title="Device">
      <Suspense fallback={<p className="muted">Loading…</p>}>
        <DeviceDetailView />
      </Suspense>
    </Shell>
  );
}
