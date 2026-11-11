"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { useAuth, useUser } from "@/components/Auth";
import { RecordForm } from "@/components/RecordForm";
import { Shell } from "@/components/Shell";
import { ErrorBox, Notice } from "@/components/ui";
import { ApiError, errorMessage } from "@/lib/api";
import { can } from "@/lib/roles";
import { emptyRecord } from "@/lib/scheme";
import type { SchemeDetail, SchemeRecord } from "@/lib/types";
import { useLoad } from "@/lib/useLoad";

/**
 * A new draft never inherits the previous version's review state: the old
 * reviewer's notes, OUTDATED reason and last_verified date do not describe
 * the new content. (The server clears verification fields too, but keeps
 * notes.)
 */
function draftFrom(r: SchemeRecord): SchemeRecord {
  const { last_verified: _lv, ...rest } = r;
  return { ...rest, verification: { status: "NEEDS_REVIEW" } };
}

function Editor({ detail }: { detail: SchemeDetail | null }) {
  const { api } = useAuth();
  const router = useRouter();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [details, setDetails] = useState<string[]>([]);
  const base = detail?.current.version ?? 0;

  async function save(r: SchemeRecord, note: string) {
    setBusy(true);
    setError(null);
    setDetails([]);
    try {
      const res = await api.saveDraft(r.id, base, r, note);
      router.push(`/schemes/detail/?id=${encodeURIComponent(res.id)}`);
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setError(
          base === 0
            ? "A scheme with this id already exists. Open it and edit it instead."
            : "Someone saved a newer version while you were editing. Copy your changes, reload, and edit the latest version.",
        );
      } else {
        setError(errorMessage(err));
        if (err instanceof ApiError) setDetails(err.details);
      }
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      {detail && (
        <Notice>
          Editing from v{detail.current.version} ({detail.current.status}). Your save becomes v
          {detail.current.version + 1} with status NEEDS_REVIEW
          {detail.current.status === "VERIFIED"
            ? "; the verified version stays on kiosks until this draft is verified"
            : ""}
          .
        </Notice>
      )}
      <RecordForm
        initial={detail ? draftFrom(detail.record) : emptyRecord()}
        isNew={!detail}
        onSave={save}
        busy={busy}
        serverError={error}
        serverDetails={details}
      />
    </>
  );
}

function EditView() {
  const { api } = useAuth();
  const user = useUser();
  const id = useSearchParams().get("id") ?? "";
  const { data, error, loading } = useLoad(
    (s) => (id ? api.scheme(id, s) : Promise.resolve(null)),
    [api, id],
  );
  if (!can(user, "editSchemes"))
    return <ErrorBox error="Your role does not allow editing schemes." />;
  return (
    <>
      <p>
        <Link href={id ? `/schemes/detail/?id=${encodeURIComponent(id)}` : "/schemes/"}>
          ← Back
        </Link>
      </p>
      <ErrorBox error={error} />
      {loading && <p className="muted">Loading…</p>}
      {!loading && !error && <Editor key={id || "new"} detail={data} />}
    </>
  );
}

export default function EditPage() {
  return (
    <Shell title="Edit scheme draft">
      <Suspense fallback={<p className="muted">Loading…</p>}>
        <EditView />
      </Suspense>
    </Shell>
  );
}
