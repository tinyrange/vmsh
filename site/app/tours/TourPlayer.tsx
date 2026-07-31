"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import type { Terminal } from "@xterm/xterm";

type TourHeader = {
  version: number;
  width: number;
  height: number;
  vmsh_tour: {
    schema: number;
    id: string;
    title: string;
    description?: string;
    vmsh_version?: string;
    commit?: string;
  };
};

type CastEvent = [number, "o" | "i" | "m", unknown];

type TourSection = {
  at: number;
  index: number;
  title: string;
  markdown: string;
};

type ParsedTour = {
  header: TourHeader;
  events: CastEvent[];
  sections: TourSection[];
  duration: number;
  transcript: string;
};

function parseTour(source: string): ParsedTour {
  const lines = source.split(/\r?\n/).filter(Boolean);
  if (lines.length === 0) throw new Error("The tour cast is empty.");

  const header = JSON.parse(lines[0]) as TourHeader;
  if (header.version !== 2 || header.vmsh_tour?.schema !== 1) {
    throw new Error("This tour uses an unsupported cast format.");
  }

  const events = lines.slice(1).map((line) => JSON.parse(line) as CastEvent);
  const sections: TourSection[] = [];
  const output: string[] = [];

  for (const event of events) {
    if (event[1] === "o" && typeof event[2] === "string") {
      output.push(event[2]);
    }
    if (event[1] !== "m" || typeof event[2] !== "object" || !event[2]) continue;
    const metadata = event[2] as {
      name?: string;
      fields?: Record<string, unknown>;
    };
    if (metadata.name !== "vmsh.tour.section" || !metadata.fields) continue;
    const { index, title, markdown } = metadata.fields;
    if (typeof title !== "string" || typeof markdown !== "string") continue;
    sections.push({
      at: event[0],
      index: typeof index === "number" ? index : sections.length + 1,
      title,
      markdown,
    });
  }

  if (sections.length === 0) throw new Error("The tour does not contain guided sections.");
  const duration = Math.max(0.1, ...events.map((event) => event[0]));
  const transcript = output
    .join("")
    .replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, "")
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\r/g, "")
    .trim();
  return { header, events, sections, duration, transcript };
}

function formatTime(seconds: number) {
  const rounded = Math.max(0, Math.floor(seconds));
  return `${Math.floor(rounded / 60)}:${String(rounded % 60).padStart(2, "0")}`;
}

export default function TourPlayer({ castUrl }: { castUrl: string }) {
  const terminalHost = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | null>(null);
  const tourRef = useRef<ParsedTour | null>(null);
  const eventIndex = useRef(0);
  const playbackStart = useRef(0);
  const playbackOffset = useRef(0);
  const frame = useRef<number | null>(null);
  const speedRef = useRef(1);
  const [tour, setTour] = useState<ParsedTour | null>(null);
  const [position, setPosition] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(1);
  const [error, setError] = useState("");

  const activeSection = useMemo(() => {
    if (!tour) return null;
    return [...tour.sections].reverse().find((section) => section.at <= position) ?? tour.sections[0];
  }, [position, tour]);

  const stop = useCallback(() => {
    setPlaying(false);
    if (frame.current !== null) cancelAnimationFrame(frame.current);
    frame.current = null;
  }, []);

  const renderUntil = useCallback((target: number, reset: boolean) => {
    const currentTour = tourRef.current;
    const currentTerminal = terminal.current;
    if (!currentTour || !currentTerminal) return;
    if (reset) {
      currentTerminal.reset();
      eventIndex.current = 0;
    }
    while (
      eventIndex.current < currentTour.events.length &&
      currentTour.events[eventIndex.current][0] <= target
    ) {
      const event = currentTour.events[eventIndex.current];
      if (event[1] === "o" && typeof event[2] === "string") {
        currentTerminal.write(event[2]);
      }
      eventIndex.current += 1;
    }
  }, []);

  const seek = useCallback(
    (target: number) => {
      stop();
      const bounded = Math.max(0, Math.min(target, tourRef.current?.duration ?? 0));
      renderUntil(bounded, true);
      setPosition(bounded);
    },
    [renderUntil, stop],
  );

  useEffect(() => {
    let cancelled = false;
    let instance: Terminal | null = null;
    async function load() {
      try {
        const response = await fetch(castUrl);
        if (!response.ok) throw new Error(`Could not load the tour (${response.status}).`);
        const parsed = parseTour(await response.text());
        if (cancelled || !terminalHost.current) return;
        const { Terminal: XTerm } = await import("@xterm/xterm");
        if (cancelled || !terminalHost.current) return;
        instance = new XTerm({
          cols: parsed.header.width,
          rows: parsed.header.height,
          convertEol: false,
          cursorBlink: false,
          disableStdin: true,
          fontFamily: "var(--font-geist-mono), ui-monospace, monospace",
          fontSize: 13,
          lineHeight: 1.22,
          scrollback: 3000,
          theme: {
            background: "#08090b",
            foreground: "#e8e8e9",
            cursor: "#80bd43",
            black: "#09090b",
            green: "#80bd43",
            brightGreen: "#a7e76b",
          },
        });
        instance.open(terminalHost.current);
        terminal.current = instance;
        tourRef.current = parsed;
        setTour(parsed);
        renderUntil(0, true);
      } catch (reason) {
        if (!cancelled) setError(reason instanceof Error ? reason.message : "Could not load this tour.");
      }
    }
    void load();
    return () => {
      cancelled = true;
      stop();
      instance?.dispose();
      terminal.current = null;
    };
  }, [castUrl, renderUntil, stop]);

  useEffect(() => {
    speedRef.current = speed;
  }, [speed]);

  const tick = useCallback(
    function advance(now: number) {
      const currentTour = tourRef.current;
      if (!currentTour) return;
      const next = playbackOffset.current + ((now - playbackStart.current) / 1000) * speedRef.current;
      const bounded = Math.min(next, currentTour.duration);
      renderUntil(bounded, false);
      setPosition(bounded);
      if (bounded >= currentTour.duration) {
        stop();
        return;
      }
      frame.current = requestAnimationFrame(advance);
    },
    [renderUntil, stop],
  );

  function togglePlayback() {
    if (!tour) return;
    if (playing) {
      stop();
      return;
    }
    let startAt = position;
    if (position >= tour.duration) {
      renderUntil(0, true);
      setPosition(0);
      startAt = 0;
    }
    playbackOffset.current = startAt;
    playbackStart.current = performance.now();
    setPlaying(true);
    frame.current = requestAnimationFrame(tick);
  }

  if (error) return <div className="tour-error" role="alert">{error}</div>;

  return (
    <div className="tour-player" aria-busy={!tour}>
      <section className="tour-terminal" aria-label="Recorded terminal session">
        <div className="tour-window-bar">
          <span className="tour-window-dots" aria-hidden="true"><i /><i /><i /></span>
          <span>{tour?.header.vmsh_tour.id ?? "Loading tour…"}.cast</span>
          <span>{tour?.header.vmsh_tour.vmsh_version ?? ""}</span>
        </div>
        <div className="tour-terminal-scroll"><div ref={terminalHost} /></div>
        <div className="tour-controls">
          <button type="button" onClick={togglePlayback} disabled={!tour} aria-label={playing ? "Pause tour" : "Play tour"}>
            {playing ? "Pause" : "Play"}
          </button>
          <input
            aria-label="Tour position"
            type="range"
            min="0"
            max={tour?.duration ?? 1}
            step="0.05"
            value={position}
            onChange={(event) => seek(Number(event.target.value))}
          />
          <span className="tour-time">{formatTime(position)} / {formatTime(tour?.duration ?? 0)}</span>
          <label>
            <span className="sr-only">Playback speed</span>
            <select value={speed} onChange={(event) => setSpeed(Number(event.target.value))}>
              <option value="0.5">0.5×</option>
              <option value="1">1×</option>
              <option value="1.5">1.5×</option>
              <option value="2">2×</option>
            </select>
          </label>
        </div>
      </section>

      <aside className="tour-guide" aria-label="Guided tour sections">
        <div className="tour-guide-heading">
          <span>Guided tour</span>
          <h2>{tour?.header.vmsh_tour.title ?? "Loading…"}</h2>
          <p>{tour?.header.vmsh_tour.description}</p>
        </div>
        <ol className="tour-sections">
          {tour?.sections.map((section) => (
            <li key={`${section.index}-${section.at}`}>
              <button
                type="button"
                className={activeSection === section ? "active" : ""}
                aria-current={activeSection === section ? "step" : undefined}
                onClick={() => seek(section.at)}
              >
                <span>{section.index}</span>
                <span>{section.title}<small>{formatTime(section.at)}</small></span>
              </button>
            </li>
          ))}
        </ol>
        <article className="tour-markdown">
          <ReactMarkdown>{activeSection?.markdown ?? "Preparing the lesson…"}</ReactMarkdown>
        </article>
        {tour ? (
          <details className="tour-transcript">
            <summary>Accessible transcript</summary>
            <pre>{tour.transcript}</pre>
          </details>
        ) : null}
      </aside>
    </div>
  );
}
