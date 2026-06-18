import "@testing-library/jest-dom";
import { vi } from "vitest";

// Polyfill EventSource for code paths that open an SSE channel
// (useEventStream). jsdom does not implement it. We stub a no-op
// constructor; nothing in tests asserts on the actual stream.
class EventSourceStub {
  url: string;
  withCredentials: boolean;
  readyState = 0;
  onopen: ((e: Event) => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  constructor(url: string, init?: EventSourceInit) {
    this.url = url;
    this.withCredentials = init?.withCredentials ?? false;
  }
  addEventListener() {}
  removeEventListener() {}
  dispatchEvent() {
    return true;
  }
  close() {}
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).EventSource = EventSourceStub;

// Polyfill ResizeObserver for recharts ResponsiveContainer.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).ResizeObserver = ResizeObserverStub;

// Mock window.matchMedia
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {}, // deprecated
    removeListener: () => {}, // deprecated
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => true,
  }),
});

// Mock monaco-editor modules
vi.mock("monaco-editor", () => ({
  default: {},
  editor: {},
}));

vi.mock("@monaco-editor/loader", () => ({
  default: {
    init: vi.fn(() => Promise.resolve()),
    config: vi.fn(),
  },
}));

vi.mock("@monaco-editor/react", () => ({
  default: vi.fn(() => null),
  Editor: vi.fn(() => null),
}));
