import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import TourPlayer from "../app/tours/TourPlayer";

const terminalState = vi.hoisted(() => ({
  instances: [] as Array<{
    writes: string[];
    resizes: Array<[number, number]>;
    resets: number;
  }>,
}));

vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    state = { writes: [] as string[], resizes: [] as Array<[number, number]>, resets: 0 };
    constructor() { terminalState.instances.push(this.state); }
    open() {}
    write(value: string) { this.state.writes.push(value); }
    resize(cols: number, rows: number) { this.state.resizes.push([cols, rows]); }
    reset() { this.state.resets += 1; }
    dispose() {}
  },
}));

const cast = [
  JSON.stringify({
    version: 2,
    width: 80,
    height: 24,
    vmsh_tour: {
      schema: 1,
      id: "player-test",
      title: "Player test",
      description: "A test lesson.",
    },
  }),
  JSON.stringify([0, "m", "Start"]),
  JSON.stringify([0, "vmsh", {
    name: "vmsh.tour.section",
    fields: { index: 1, title: "Start", markdown: "Type **carefully**. <script>alert('no')</script>" },
  }]),
  JSON.stringify([0.1, "o", "host $ "]),
  JSON.stringify([0.2, "i", "echo hello"]),
  JSON.stringify([0.3, "i", "\r"]),
  JSON.stringify([0.4, "o", "echo hello\r\nhello\r\n"]),
  JSON.stringify([1, "m", "Resize"]),
  JSON.stringify([1, "vmsh", {
    name: "vmsh.tour.section",
    fields: { index: 2, title: "Resize", markdown: "The terminal becomes wider." },
  }]),
  JSON.stringify([1.1, "r", "100x30"]),
  JSON.stringify([1.2, "o", "done\r\n"]),
].join("\n");

describe("TourPlayer", () => {
  beforeEach(() => {
    terminalState.instances.length = 0;
    vi.stubGlobal("fetch", vi.fn(async () => new Response(cast, { status: 200 })));
    vi.stubGlobal("requestAnimationFrame", vi.fn(() => 1));
    vi.stubGlobal("cancelAnimationFrame", vi.fn());
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  test("loads, seeks by section, and replays standard resize events", async () => {
    const user = userEvent.setup();
    render(<TourPlayer castUrl="/tour.cast" />);

    await screen.findByRole("button", { name: "Play tour" });
    await user.click(screen.getByRole("button", { name: /Resize/ }));
    expect(screen.getByRole("button", { name: /Resize/ }).getAttribute("aria-current")).toBe("step");

    fireEvent.change(screen.getByRole("slider", { name: "Tour position" }), {
      target: { value: "1.2" },
    });
    await waitFor(() => expect(terminalState.instances[0].resizes).toContainEqual([100, 30]));
    expect(terminalState.instances[0].writes.join("")).toContain("done");
  });

  test("supports keyboard playback and navigation", async () => {
    const user = userEvent.setup();
    render(<TourPlayer castUrl="/tour.cast" />);
    await screen.findByRole("button", { name: "Play tour" });

    const player = screen.getByLabelText("Tour playback");
    player.focus();
    await user.keyboard(" ");
    expect(screen.getByRole("button", { name: "Pause tour" })).toBeTruthy();
    await user.keyboard("{ArrowRight}");
    expect((screen.getByRole("slider", { name: "Tour position" }) as HTMLInputElement).value).toBe("1.2");
  });

  test("renders a structured, sanitized transcript", async () => {
    const { container } = render(<TourPlayer castUrl="/tour.cast" />);
    await screen.findByRole("heading", { name: "Read the lesson without replaying it" });

    expect(screen.getByRole("heading", { name: "Start" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Commands entered" })).toBeTruthy();
    expect(screen.getByText("echo hello", { selector: "code" })).toBeTruthy();
    expect(screen.getByLabelText("Terminal output for step 1").textContent).toContain("hello");
    expect(screen.getAllByText("carefully", { selector: "strong" })).toHaveLength(2);
    expect(container.querySelector("script")).toBeNull();
  });
});
