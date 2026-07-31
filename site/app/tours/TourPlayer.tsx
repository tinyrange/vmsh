"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import type { Terminal } from "@xterm/xterm";
import { parseTour, resizeForEvent, type ParsedTour } from "./tour-model";

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
      currentTerminal.resize(currentTour.header.width, currentTour.header.height);
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
      const resize = resizeForEvent(event);
      if (resize) currentTerminal.resize(resize.cols, resize.rows);
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

  function handlePlayerKeyDown(event: React.KeyboardEvent<HTMLDivElement>) {
    if (event.target !== event.currentTarget || !tour) return;
    if (event.key === " ") {
      event.preventDefault();
      togglePlayback();
    } else if (event.key === "ArrowLeft") {
      event.preventDefault();
      seek(position - 5);
    } else if (event.key === "ArrowRight") {
      event.preventDefault();
      seek(position + 5);
    }
  }

  if (error) return <div className="tour-error" role="alert">{error}</div>;

  return (
    <>
    <div
      className="tour-player"
      aria-busy={!tour}
      aria-label="Tour playback"
      onKeyDown={handlePlayerKeyDown}
      tabIndex={0}
    >
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
      </aside>
    </div>
    {tour ? (
      <section className="tour-transcript" aria-labelledby="tour-transcript-heading">
        <p className="kicker"><span /> ACCESSIBLE TRANSCRIPT</p>
        <h2 id="tour-transcript-heading">Read the lesson without replaying it</h2>
        <p className="tour-transcript-intro">
          Each step includes its instruction, recorded commands, and a text version of
          the terminal output. Times identify the matching point in the replay.
        </p>
        <ol>
          {tour.transcript.map((section) => (
            <li key={`${section.index}-${section.at}`}>
              <article aria-labelledby={`transcript-step-${section.index}`}>
                <header>
                  <span>Step {section.index}</span>
                  <time dateTime={`PT${section.at.toFixed(3)}S`}>{formatTime(section.at)}</time>
                  <h3 id={`transcript-step-${section.index}`}>{section.title}</h3>
                </header>
                <div className="tour-transcript-instruction"><ReactMarkdown>{section.markdown}</ReactMarkdown></div>
                {section.commands.length ? (
                  <div className="tour-transcript-commands">
                    <h4>Commands entered</h4>
                    <ul>{section.commands.map((command, index) => <li key={`${command}-${index}`}><code>{command}</code></li>)}</ul>
                  </div>
                ) : null}
                {section.output ? (
                  <details>
                    <summary>Terminal output for {section.title}</summary>
                    <pre tabIndex={0} aria-label={`Terminal output for step ${section.index}`}>{section.output}</pre>
                  </details>
                ) : null}
              </article>
            </li>
          ))}
        </ol>
      </section>
    ) : null}
    </>
  );
}
