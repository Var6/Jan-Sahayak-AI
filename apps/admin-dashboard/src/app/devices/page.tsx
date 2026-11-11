"use client";

import Link from "next/link";
import { type FormEvent, useEffect, useState } from "react";
import { useAuth } from "@/components/Auth";
import { Can, Shell } from "@/components/Shell";
import { ConnectivityBadge, ErrorBox, HealthBadge } from "@/components/ui";
import { ApiError, errorMessage } from "@/lib/api";
import { connectivity, validateNewDevice } from "@/lib/devices";
import { fmtAge, fmtBytes, fmtTime } from "@/lib/format";
import type { DeviceListItem, HeartbeatRecord } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

const PAGE_SIZE = 25;
/** Detail requests in flight at once when filling resource columns. */
const DETAIL_CONCURRENCY = 3;

function RegisterDevice({ onDone }: { onDone: () => void }) {
  const { api } = useAuth();
  const [f, setF] = useState({ id: "", public_key: "", name: "", district: "" });
  const [errors, setErrors] = useState<string[]>([]);
  const [serverError, setServerError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [ok, setOk] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setOk(null);
    setServerError(null);
    const v = validateNewDevice(f);
    setErrors(v);
    if (v.length) return;
    setBusy(true);
    try {
      const r = await api.registerDevice({
        id: f.id.trim(),
        public_key: f.public_key.trim(),
        name: f.name.trim(),
        district: f.district.trim(),
      });
      setOk(`Registered ${r.device.name}.`);
      setF({ id: "", public_key: "", name: "", district: "" });
      onDone();
    } catch (err) {
      setServerError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <details className="panel">
      <summary>Register a device</summary>
      <form onSubmit={onSubmit} className="stack" noValidate>
        <p className="muted small">
          Copy the id and public key from the device identity file created at provisioning. The
          private key never leaves the device.
        </p>
        <label>
          Device id (UUID)
          <input
            value={f.id}
            onChange={(e) => setF({ ...f, id: e.target.value })}
            spellCheck={false}
          />
        </label>
        <label>
          Public key (base64 Ed25519)
          <input
            value={f.public_key}
            onChange={(e) => setF({ ...f, public_key: e.target.value })}
            spellCheck={false}
          />
        </label>
        <label>
          Name
          <input
            value={f.name}
            maxLength={100}
            onChange={(e) => setF({ ...f, name: e.target.value })}
          />
        </label>
        <label>
          District
          <input
            value={f.district}
            maxLength={60}
            onChange={(e) => setF({ ...f, district: e.target.value })}
          />
        </label>
        <ErrorBox error={errors.length ? "Please fix:" : null} details={errors} />
        <ErrorBox error={serverError} />
        {ok && <p role="status">{ok}</p>}
        <button type="submit" disabled={busy}>
          {busy ? "Registering…" : "Register device"}
        </button>
      </form>
    </details>
  );
}

function pct(free?: number, total?: number): string {
  if (free === undefined || total === undefined || total <= 0) return "—";
  return `${fmtBytes(free)} free (${Math.round((free / total) * 100)}%)`;
}

function Devices() {
  const { api } = useAuth();
  const { data, error, loading, reload } = useLoad((s) => api.devices(s), [api]);
  const [page, setPage] = useState(0);
  const [details, setDetails] = useState<Record<string, HeartbeatRecord | null>>({});
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);

  const devices: DeviceListItem[] = data?.devices ?? [];
  const pages = Math.max(1, Math.ceil(devices.length / PAGE_SIZE));
  const visible = devices.slice(page * PAGE_SIZE, page * PAGE_SIZE + PAGE_SIZE);
  const visibleKey = visible.map((d) => `${d.id}:${d.last_heartbeat?.received_at ?? ""}`).join(",");

  // The list endpoint carries only a heartbeat summary; disk, memory,
  // temperature and last sync come from the device detail. Fetch them for
  // the visible page only, a few at a time.
  // biome-ignore lint/correctness/useExhaustiveDependencies: visibleKey captures the visible rows
  useEffect(() => {
    const ctrl = new AbortController();
    const queue = visible.filter((d) => d.last_heartbeat).map((d) => d.id);
    const worker = async () => {
      for (let id = queue.shift(); id; id = queue.shift()) {
        try {
          const r = await api.device(id, ctrl.signal);
          if (!ctrl.signal.aborted) setDetails((m) => ({ ...m, [id]: r.last_heartbeat ?? null }));
        } catch (e) {
          if (ctrl.signal.aborted) return;
          if (!(e instanceof ApiError)) return;
          setDetails((m) => ({ ...m, [id]: null }));
        }
      }
    };
    for (let i = 0; i < DETAIL_CONCURRENCY; i++) void worker();
    return () => ctrl.abort();
  }, [api, visibleKey]);

  return (
    <>
      <Can cap="manageDevices">
        <RegisterDevice onDone={reload} />
      </Can>
      <div className="toolbar">
        <button type="button" onClick={reload} disabled={loading}>
          Refresh
        </button>
        <span className="muted small">
          Online = heartbeat within 15 min. Times in IST.{" "}
          {data ? `${devices.length} device(s).` : ""}
        </span>
      </div>
      <ErrorBox error={error} />
      {loading && !data && <p className="muted">Loading devices…</p>}
      {data && devices.length === 0 && <p>No devices registered yet.</p>}
      {visible.length > 0 && (
        <div className="table-scroll">
          <table className="devices">
            <caption className="sr-only">Registered devices</caption>
            <thead>
              <tr>
                <th scope="col">Device</th>
                <th scope="col">Status</th>
                <th scope="col">Health</th>
                <th scope="col">Last heartbeat</th>
                <th scope="col">Agent / bundle</th>
                <th scope="col" className="num">
                  Outbox pending
                </th>
                <th scope="col">Disk</th>
                <th scope="col">Memory</th>
                <th scope="col" className="num">
                  CPU °C
                </th>
                <th scope="col">Last sync</th>
              </tr>
            </thead>
            <tbody>
              {visible.map((d) => {
                const hb = d.last_heartbeat;
                const det = details[d.id];
                const failed = hb ? hb.outbox_failed + hb.outbox_conflict : 0;
                return (
                  <tr key={d.id}>
                    <td>
                      <Link href={`/devices/detail/?id=${encodeURIComponent(d.id)}`}>{d.name}</Link>
                      <div className="muted small">{d.district}</div>
                    </td>
                    <td>
                      <ConnectivityBadge c={connectivity(d, now)} />
                    </td>
                    <td>
                      <HealthBadge state={hb?.overall_health} />
                    </td>
                    <td>
                      {hb ? (
                        <>
                          {fmtAge(now - Date.parse(hb.received_at))}
                          <div className="muted small">{fmtTime(hb.received_at)}</div>
                        </>
                      ) : (
                        "—"
                      )}
                    </td>
                    <td>
                      {hb?.agent_version || "—"}
                      <div className="muted small">{hb?.bundle_version || "no bundle"}</div>
                    </td>
                    <td className="num">
                      {hb ? hb.outbox_pending : "—"}
                      {failed > 0 && <div className="small bad-text">{failed} failed/conflict</div>}
                    </td>
                    <td>{det ? pct(det.disk_free_bytes, det.disk_total_bytes) : hb ? "…" : "—"}</td>
                    <td>
                      {det ? pct(det.mem_available_bytes, det.mem_total_bytes) : hb ? "…" : "—"}
                    </td>
                    <td className="num">
                      {det?.cpu_temp_c !== undefined
                        ? det.cpu_temp_c.toFixed(0)
                        : hb && det === undefined
                          ? "…"
                          : "—"}
                      {det?.throttled && <div className="small bad-text">throttled</div>}
                    </td>
                    <td>{det ? fmtTime(det.sync_last_success_at) : hb ? "…" : "—"}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {pages > 1 && (
        <nav className="pager" aria-label="Device pages">
          <button type="button" disabled={page === 0} onClick={() => setPage(page - 1)}>
            Previous
          </button>
          <span>
            Page {page + 1} of {pages}
          </span>
          <button type="button" disabled={page >= pages - 1} onClick={() => setPage(page + 1)}>
            Next
          </button>
        </nav>
      )}
    </>
  );
}

export default function DevicesPage() {
  return (
    <Shell title="Devices">
      <Devices />
    </Shell>
  );
}
