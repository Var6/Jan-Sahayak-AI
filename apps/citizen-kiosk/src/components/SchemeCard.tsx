import type { Card, Lang } from "../api/types";
import { formatDate } from "../format";
import { STRINGS, t } from "../i18n";

/** Badge text in the UI language first, then the other language. */
function badge(lang: Lang, verified: boolean): string {
  const other: Lang = lang === "hi" ? "en" : "hi";
  const k = verified ? "verified" : "unverified";
  return `${STRINGS[lang][k]} · ${STRINGS[other][k]}`;
}

export function SchemeCard({ card, lang }: { card: Card; lang: Lang }) {
  const s = t(lang);
  const date = formatDate(card.last_verified);
  const headingId = `card-${card.scheme_id}`;
  return (
    <article
      className={`card ${card.verified ? "card--verified" : "card--unverified"}`}
      aria-labelledby={headingId}
      data-testid="scheme-card"
    >
      <header className="card__head">
        <h3 id={headingId} className="card__title">
          {card.title}
        </h3>
        <span
          className={`badge ${card.verified ? "badge--ok" : "badge--pending"}`}
          data-testid="verify-badge"
        >
          <span aria-hidden="true">{card.verified ? "✓ " : "! "}</span>
          {badge(lang, card.verified)}
        </span>
      </header>
      {date && (
        <p className="card__date">
          {s.lastVerified}: <strong>{date}</strong>
        </p>
      )}
      {!card.verified && <p className="card__warn">{s.unverifiedNote}</p>}
      {card.sections.map((sec, i) => (
        // Sections have no ids and titles may repeat; the list is replaced
        // wholesale with each reply, so position is a stable key.
        // biome-ignore lint/suspicious/noArrayIndexKey: static per reply
        <section className="card__section" key={`${i}-${sec.title}`}>
          <h4>{sec.title}</h4>
          <ul>
            {sec.items.map((it, j) => (
              // biome-ignore lint/suspicious/noArrayIndexKey: static per reply
              <li key={`${j}-${it}`}>{it}</li>
            ))}
          </ul>
        </section>
      ))}
      {(card.helpline || card.official_url) && (
        <dl className="card__contact">
          {card.helpline && (
            <div>
              <dt>{s.helpline}</dt>
              <dd className="card__helpline">{card.helpline}</dd>
            </div>
          )}
          {card.official_url && (
            <div>
              <dt>{s.officialSite}</dt>
              {/* Plain text: the kiosk never navigates away. */}
              <dd className="card__url">{card.official_url}</dd>
            </div>
          )}
        </dl>
      )}
      {card.disclaimer && <p className="card__disclaimer">{card.disclaimer}</p>}
    </article>
  );
}
