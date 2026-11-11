import Link from "next/link";

export default function NotFound() {
  return (
    <main id="main" className="narrow">
      <h1>Page not found</h1>
      <p>
        <Link href="/">Back to the overview</Link>
      </p>
    </main>
  );
}
