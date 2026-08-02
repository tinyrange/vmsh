import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import TourPlayer from "../TourPlayer";
import { findTour, releaseTag, tourAssetUrl, tours } from "../catalog";

export function generateStaticParams() {
  return tours.map((tour) => ({ id: tour.id }));
}

export async function generateMetadata(
  { params }: { params: Promise<{ id: string }> },
): Promise<Metadata> {
  const tour = findTour((await params).id);
  if (!tour) return {};
  return { title: `${tour.title} — vmsh`, description: tour.description };
}

export default async function TourPage({ params }: { params: Promise<{ id: string }> }) {
  const tour = findTour((await params).id);
  if (!tour) notFound();
  const publicationLabel = tour.released ? `Release ${releaseTag}` : "Unreleased preview";

  return (
    <main className="tour-page">
      <header className="site-header">
        <Link className="vmsh-logo" href="/" aria-label="vmsh home">
          <span className="prompt-mark">&gt;_</span>
          <span>vmsh</span>
        </Link>
        <nav aria-label="Tour navigation">
          <Link href="/tours">All tours</Link>
          <a className="github-link" href={`https://github.com/tinyrange/vmsh/blob/main/tours/${tour.id}.star`}>
            Tour source <span aria-hidden="true">↗</span>
          </a>
        </nav>
      </header>

      <section className="tour-hero">
        <p className="kicker"><span /> TESTED DOCUMENTATION</p>
        <h1>{tour.title}.</h1>
        <p>{tour.description}</p>
        <div className="tour-provenance">
          <span>{publicationLabel}</span>
          <span>Real PTY session</span>
          <span>Behavior verified</span>
        </div>
      </section>

      <TourPlayer castUrl={tourAssetUrl(tour)} />

      {tour.proofs?.length ? (
        <section className="tour-followup">
          <p className="kicker"><span /> WHAT THIS PROVES</p>
          <h2>One recorded story, verified behavior.</h2>
          <div>{tour.proofs.map((proof) => <p key={proof}>{proof}</p>)}</div>
        </section>
      ) : null}
    </main>
  );
}
