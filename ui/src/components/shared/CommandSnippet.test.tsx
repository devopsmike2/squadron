import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

import { CommandSnippet } from "./CommandSnippet";

const writeText = vi.fn().mockResolvedValue(undefined);

beforeEach(() => {
  writeText.mockClear();
  Object.assign(navigator, { clipboard: { writeText } });
});

describe("CommandSnippet", () => {
  it("renders the command in the block variant", () => {
    render(<CommandSnippet command="squadronctl agents list" />);
    expect(screen.getByText("squadronctl agents list")).toBeInTheDocument();
  });

  it("copies the command to the clipboard when Copy is clicked", async () => {
    render(<CommandSnippet command="squadronctl agents restart agent-1" />);
    fireEvent.click(
      screen.getByRole("button", {
        name: /Copy command: squadronctl agents restart agent-1/,
      }),
    );
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "squadronctl agents restart agent-1",
      ),
    );
    // Copied confirmation swaps in.
    expect(await screen.findByText("Copied")).toBeInTheDocument();
  });

  it("exposes the command in the inline variant and copies it", async () => {
    render(
      <CommandSnippet
        variant="inline"
        command="squadronctl agents clear-config agent-2"
      />,
    );
    const button = screen.getByRole("button", {
      name: /Copy command: squadronctl agents clear-config agent-2/,
    });
    // Command text is present in the DOM (visually hidden) for a11y/tests.
    expect(
      screen.getByText("squadronctl agents clear-config agent-2"),
    ).toBeInTheDocument();
    fireEvent.click(button);
    await waitFor(() =>
      expect(writeText).toHaveBeenCalledWith(
        "squadronctl agents clear-config agent-2",
      ),
    );
  });
});
