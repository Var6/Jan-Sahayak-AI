// Document check: consent → camera preview → photo → "is this a <type>?"
// → which of the scheme's documents seem covered.
//
// Privacy rules enforced here:
//   * the camera opens only after the agent accepted the consent POST;
//   * video only (no microphone), and every track is stopped right after
//     the photo is taken, on any error, and when this screen goes away;
//   * the photo is sent once to the device agent and not kept in the UI;
//   * leaving the screen withdraws the consent (the agent then forgets
//     what it read).

import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, type DeviceApi } from "../api/client";
import type { ConfirmResult, CoverageItem, DocumentResult, Lang } from "../api/types";
import { type Camera, CameraError, stopStream } from "../camera/camera";
import { t } from "../i18n";

type Step =
  | { k: "consent" }
  | { k: "opening" }
  | { k: "preview" }
  | { k: "reading" }
  | { k: "result"; res: DocumentResult }
  | { k: "coverage"; res: ConfirmResult }
  | { k: "error"; msg: string; retry: "camera" | "consent" | "none" };

export interface DocumentCheckProps {
  api: DeviceApi;
  camera: Camera;
  sessionId: string;
  lang: Lang;
  /** Citizen interaction (keeps the session alive). */
  onActivity?: () => void;
  /** The screen is closing (done, cancelled, or the session is gone). */
  onClose: () => void;
}

export function DocumentCheck({
  api,
  camera,
  sessionId,
  lang,
  onActivity,
  onClose,
}: DocumentCheckProps) {
  const s = t(lang).doc;
  const [step, setStep] = useState<Step>({ k: "consent" });
  const [busy, setBusy] = useState(false);
  const streamRef = useRef<MediaStream | null>(null);
  const videoRef = useRef<HTMLVideoElement | null>(null);
  const consented = useRef(false);
  const alive = useRef(true);

  const stopCamera = useCallback(() => {
    stopStream(streamRef.current);
    streamRef.current = null;
    const v = videoRef.current;
    if (v) v.srcObject = null;
  }, []);

  // Leaving the screen (close, session end, unmount): camera off, consent
  // withdrawn so the agent drops what it read.
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      stopCamera();
      if (consented.current) {
        consented.current = false;
        api.cameraConsent(sessionId, false).catch(() => {});
      }
    };
  }, [api, sessionId, stopCamera]);

  const fail = useCallback(
    (e: unknown, retry: "camera" | "consent" | "none") => {
      stopCamera();
      if (!alive.current) return;
      if (e instanceof ApiError && e.sessionGone) {
        onClose();
        return;
      }
      let msg = s.error;
      if (e instanceof CameraError) msg = e.code === "denied" ? s.denied : s.unavailable;
      else if (e instanceof ApiError && e.code === "unavailable") msg = s.unavailable;
      if (e instanceof ApiError && e.code === "no_consent") retry = "consent";
      setStep({ k: "error", msg, retry });
    },
    [onClose, s, stopCamera],
  );

  // Attach the stream to the preview element once it is rendered.
  useEffect(() => {
    if (step.k !== "preview") return;
    const v = videoRef.current;
    const stream = streamRef.current;
    if (!v || !stream) return;
    try {
      v.srcObject = stream;
    } catch (e) {
      fail(
        new CameraError("unavailable", e instanceof Error ? e.message : "preview failed"),
        "camera",
      );
      return;
    }
    const p = v.play?.();
    if (p && typeof p.catch === "function") p.catch(() => {});
  }, [step.k, fail]);

  const openCamera = useCallback(async () => {
    setStep({ k: "opening" });
    let stream: MediaStream;
    try {
      stream = await camera.open();
    } catch (e) {
      fail(e, "camera");
      return;
    }
    if (!alive.current) {
      stopStream(stream);
      return;
    }
    streamRef.current = stream;
    setStep({ k: "preview" });
  }, [camera, fail]);

  const agree = async () => {
    if (busy) return;
    onActivity?.();
    setBusy(true);
    try {
      await api.cameraConsent(sessionId, true);
      consented.current = true;
    } catch (e) {
      setBusy(false);
      fail(e, "none");
      return;
    }
    setBusy(false);
    if (alive.current) await openCamera();
  };

  const capture = async () => {
    const v = videoRef.current;
    if (busy || !v) return;
    onActivity?.();
    setBusy(true);
    let jpeg: Blob;
    try {
      jpeg = await camera.capture(v);
    } catch (e) {
      setBusy(false);
      fail(e, "camera");
      return;
    } finally {
      // The photo is taken: the camera goes off at once.
      stopCamera();
    }
    setStep({ k: "reading" });
    try {
      const res = await api.sendDocument(sessionId, jpeg);
      if (alive.current) setStep({ k: "result", res });
    } catch (e) {
      fail(e, "camera");
    } finally {
      setBusy(false);
    }
  };

  const answer = async (correct: boolean) => {
    if (busy) return;
    onActivity?.();
    setBusy(true);
    try {
      const res = await api.confirmDocument(sessionId, correct);
      if (!alive.current) return;
      if (correct) setStep({ k: "coverage", res });
      else await openCamera();
    } catch (e) {
      fail(e, "camera");
    } finally {
      setBusy(false);
    }
  };

  const close = () => {
    stopCamera();
    onClose();
  };

  return (
    <section className="doc" aria-labelledby="doc-title" data-testid="doc-check">
      <h2 id="doc-title" className="doc__title">
        {step.k === "consent" ? s.consentTitle : s.open}
      </h2>

      {step.k === "consent" && (
        <>
          <ul className="doc__consent" data-testid="doc-consent">
            {s.consent.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
          <div className="doc__actions">
            <button
              type="button"
              className="btn btn--primary"
              onClick={() => void agree()}
              disabled={busy}
              data-testid="doc-agree"
            >
              {s.agree}
            </button>
            <button type="button" className="btn" onClick={close} data-testid="doc-cancel">
              {s.cancel}
            </button>
          </div>
        </>
      )}

      {step.k === "opening" && (
        <p className="doc__status" role="status">
          {s.opening}
        </p>
      )}

      {step.k === "preview" && (
        <>
          <p className="doc__hint">{s.aim}</p>
          <video
            ref={videoRef}
            className="doc__video"
            autoPlay
            playsInline
            muted
            data-testid="doc-video"
          />
          <div className="doc__actions">
            <button
              type="button"
              className="btn btn--primary"
              onClick={() => void capture()}
              disabled={busy}
              data-testid="doc-capture"
            >
              {s.capture}
            </button>
            <button type="button" className="btn" onClick={close}>
              {s.cancel}
            </button>
          </div>
        </>
      )}

      {step.k === "reading" && (
        <p className="doc__status" role="status" data-testid="doc-reading">
          {s.reading}
        </p>
      )}

      {step.k === "result" && (
        <>
          <p className="doc__question" data-testid="doc-question">
            {step.res.text}
          </p>
          {step.res.can_confirm && <Fields res={step.res} lang={lang} />}
          <p className="doc__notice" data-testid="doc-notice">
            {step.res.notice}
          </p>
          <div className="doc__actions">
            {step.res.can_confirm ? (
              <>
                <button
                  type="button"
                  className="btn btn--primary"
                  onClick={() => void answer(true)}
                  disabled={busy}
                  data-testid="doc-yes"
                >
                  {s.yes}
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={() => void answer(false)}
                  disabled={busy}
                  data-testid="doc-retake"
                >
                  {s.retake}
                </button>
              </>
            ) : (
              <button
                type="button"
                className="btn btn--primary"
                onClick={() => void openCamera()}
                data-testid="doc-retake"
              >
                {s.tryAgain}
              </button>
            )}
            <button type="button" className="btn" onClick={close}>
              {s.done}
            </button>
          </div>
        </>
      )}

      {step.k === "coverage" && (
        <>
          <CoverageView res={step.res} lang={lang} />
          <div className="doc__actions">
            <button
              type="button"
              className="btn btn--primary"
              onClick={() => void openCamera()}
              data-testid="doc-another"
            >
              {s.another}
            </button>
            <button type="button" className="btn" onClick={close} data-testid="doc-done">
              {s.done}
            </button>
          </div>
        </>
      )}

      {step.k === "error" && (
        <>
          <p className="doc__error" role="alert" data-testid="doc-error">
            {step.msg}
          </p>
          <div className="doc__actions">
            {step.retry === "camera" && consented.current && (
              <button type="button" className="btn btn--primary" onClick={() => void openCamera()}>
                {s.tryAgain}
              </button>
            )}
            {step.retry === "consent" && (
              <button
                type="button"
                className="btn btn--primary"
                onClick={() => {
                  consented.current = false;
                  setStep({ k: "consent" });
                }}
              >
                {s.tryAgain}
              </button>
            )}
            <button type="button" className="btn" onClick={close} data-testid="doc-close">
              {s.done}
            </button>
          </div>
        </>
      )}
    </section>
  );
}

function Fields({ res, lang }: { res: DocumentResult; lang: Lang }) {
  const s = t(lang).doc;
  const d = res.document;
  const rows: [string, string][] = [];
  if (d.name) rows.push([s.fieldName, d.name]);
  if (d.issuing_authority) rows.push([s.fieldAuthority, d.issuing_authority]);
  if (d.issue_date) rows.push([s.fieldDate, d.issue_date]);
  for (const id of d.masked_ids ?? []) rows.push([s.fieldIds, id.value]);
  if (rows.length === 0) return null;
  return (
    <dl className="doc__fields" data-testid="doc-fields">
      {rows.map(([k, v]) => (
        <div key={`${k}:${v}`} className="doc__field">
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

const statusClass: Record<CoverageItem["status"], string> = {
  seen: "doc__item--seen",
  not_seen: "doc__item--missing",
  not_checkable: "doc__item--na",
};

function CoverageView({ res, lang }: { res: ConfirmResult; lang: Lang }) {
  const s = t(lang).doc;
  const label: Record<CoverageItem["status"], string> = {
    seen: s.seen,
    not_seen: s.notSeen,
    not_checkable: s.notCheckable,
  };
  const cov = res.coverage;
  const confirmed = res.confirmed ?? [];
  return (
    <div className="doc__coverage" data-testid="doc-coverage">
      {confirmed.length > 0 && (
        <p className="doc__shown">
          {s.shown}: {confirmed.map((c) => c.label).join(", ")}
        </p>
      )}
      {!cov && <p className="doc__message">{res.text}</p>}
      {cov && (
        <>
          <h3 className="doc__scheme">{s.docsFor(cov.scheme_name)}</h3>
          {cov.disclaimer && <p className="doc__disclaimer">{cov.disclaimer}</p>}
          {cov.message && (
            <p className="doc__message" data-testid="doc-coverage-message">
              {cov.message}
            </p>
          )}
          {cov.items && cov.items.length > 0 && (
            <ul className="doc__items">
              {cov.items.map((it, i) => (
                <li
                  // biome-ignore lint/suspicious/noArrayIndexKey: names may repeat
                  key={i}
                  className={`doc__item ${statusClass[it.status]}`}
                  data-testid={`doc-item-${it.status}`}
                >
                  <span className="doc__item-name">
                    {it.name} {it.optional && <span className="doc__opt">{s.optional}</span>}
                  </span>
                  <span className="doc__item-status">{label[it.status]}</span>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {res.caveat && <p className="doc__caveat">{res.caveat}</p>}
      <p className="doc__notice">{res.notice}</p>
    </div>
  );
}
