import "@testing-library/jest-dom/vitest";
import { afterAll, afterEach, beforeAll } from "vitest";
import { cleanup } from "@testing-library/react";
import { server } from "./server";

// jsdom gaps used by Radix and the theme provider.
if (!window.matchMedia) {
  window.matchMedia = (query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as MediaQueryList;
}
class RO {
  observe() {}
  unobserve() {}
  disconnect() {}
}
(globalThis as unknown as { ResizeObserver: typeof RO }).ResizeObserver ??= RO;
Element.prototype.scrollIntoView ??= () => {};

// jsdom's AbortSignal is not the class Node's fetch (undici) expects, so a
// query passing `signal` would fail under test. Drop it here only; request
// cancellation itself is a browser behaviour covered by the e2e suite.
// Installed after server.listen(), which patches fetch itself.
function stripSignals() {
  const patched = globalThis.fetch;
  globalThis.fetch = (input: RequestInfo | URL, init?: RequestInit) =>
    patched(input, init ? { ...init, signal: undefined } : init);
}

beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
  stripSignals();
});
afterEach(() => {
  server.resetHandlers();
  cleanup();
});
afterAll(() => server.close());
