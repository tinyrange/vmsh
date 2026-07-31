import Link from "next/link";
import release from "../../release-data.json";
import TourPlayer from "../TourPlayer";

export const metadata = {
  title: "Context switching tour — vmsh",
  description: "A tested guided tour of moving between host and VM contexts in vmsh.",
};

export default function ContextSwitchingTour() {
  const publishedTours = release.tours as Array<{ id: string; title: string; url: string }>;
  const publishedTour = publishedTours.find((tour) => tour.id === "context-switching");
  const basePath = process.env.NEXT_PUBLIC_VMSH_BASE_PATH ?? "";
  const castUrl = publishedTour?.url
    ? `${basePath}${publishedTour.url}`
    : `${basePath}/tours/context-switching.cast`;
  const publicationLabel = publishedTour ? `Release ${release.tag}` : "Unreleased preview";

  return (
    <main className="tour-page">
      <header className="site-header">
        <Link className="vmsh-logo" href="/" aria-label="vmsh home">
          <span className="prompt-mark">&gt;_</span>
          <span>vmsh</span>
        </Link>
        <nav aria-label="Tour navigation">
          <Link href="/">Downloads</Link>
          <a className="github-link" href="https://github.com/tinyrange/vmsh/tree/main/tours">
            Tour source <span aria-hidden="true">↗</span>
          </a>
        </nav>
      </header>

      <section className="tour-hero">
        <p className="kicker"><span /> TESTED DOCUMENTATION</p>
        <h1>Move between host and VM contexts.</h1>
        <p>
          This lesson was produced by an automated session that exercised the same
          interactive vmsh workflow shown below. Select a section or replay it from
          the beginning.
        </p>
        <div className="tour-provenance">
          <span>{publicationLabel}</span>
          <span>Real PTY session</span>
          <span>Behavior verified</span>
        </div>
      </section>

      <TourPlayer castUrl={castUrl} />

      <section className="tour-followup">
        <p className="kicker"><span /> WHAT THIS PROVES</p>
        <h2>One shell context, several systems.</h2>
        <div>
          <p>vmsh waits for a selected VM to become ready before returning control.</p>
          <p>Ordinary commands run in the selected system without repeating a target.</p>
          <p>Returning to the host does not discard the VM&apos;s persistent shell state.</p>
        </div>
      </section>
    </main>
  );
}
