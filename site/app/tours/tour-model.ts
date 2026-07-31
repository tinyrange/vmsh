export type TourHeader = {
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

export type CastEvent = [number, "o" | "i" | "m" | "r" | "vmsh", unknown];

export type TourSection = {
  at: number;
  index: number;
  title: string;
  markdown: string;
};

export type TranscriptSection = TourSection & {
  commands: string[];
  output: string;
};

export type ParsedTour = {
  header: TourHeader;
  events: CastEvent[];
  sections: TourSection[];
  transcript: TranscriptSection[];
  duration: number;
};

type Metadata = { name?: string; fields?: Record<string, unknown> };

function metadataFor(event: CastEvent): Metadata | null {
  if (event[1] !== "vmsh" && event[1] !== "m") return null;
  if (typeof event[2] !== "object" || !event[2]) return null;
  return event[2] as Metadata;
}

export function stripTerminalControl(value: string) {
  return value
    .replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, "")
    .replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "\n")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function transcriptFor(events: CastEvent[], sections: TourSection[]) {
  return sections.map((section, index): TranscriptSection => {
    const until = sections[index + 1]?.at ?? Number.POSITIVE_INFINITY;
    const sectionEvents = events.filter((event) => event[0] >= section.at && event[0] < until);
    const input = sectionEvents
      .filter((event) => event[1] === "i" && typeof event[2] === "string")
      .map((event) => event[2] as string)
      .join("");
    const commands = input
      .split(/\r|\n/)
      .map((command) => stripTerminalControl(command).replace(/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/g, ""))
      .filter(Boolean);
    const output = stripTerminalControl(sectionEvents
      .filter((event) => event[1] === "o" && typeof event[2] === "string")
      .map((event) => event[2] as string)
      .join(""));
    return { ...section, commands, output };
  });
}

export function parseTour(source: string): ParsedTour {
  const lines = source.split(/\r?\n/).filter(Boolean);
  if (lines.length === 0) throw new Error("The tour cast is empty.");

  const header = JSON.parse(lines[0]) as TourHeader;
  if (header.version !== 2 || header.vmsh_tour?.schema !== 1) {
    throw new Error("This tour uses an unsupported cast format.");
  }

  const events = lines.slice(1).map((line) => JSON.parse(line) as CastEvent);
  const sections: TourSection[] = [];
  for (const event of events) {
    const metadata = metadataFor(event);
    if (metadata?.name !== "vmsh.tour.section" || !metadata.fields) continue;
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
  return { header, events, sections, transcript: transcriptFor(events, sections), duration };
}

export function resizeForEvent(event: CastEvent): { cols: number; rows: number } | null {
  if (event[1] === "r" && typeof event[2] === "string") {
    const match = /^(\d+)x(\d+)$/.exec(event[2]);
    if (match) return { cols: Number(match[1]), rows: Number(match[2]) };
  }
  const metadata = metadataFor(event);
  if (metadata?.name !== "ptyterm.resize" || !metadata.fields) return null;
  const { cols, rows } = metadata.fields;
  return typeof cols === "number" && typeof rows === "number" && cols > 0 && rows > 0
    ? { cols, rows }
    : null;
}
