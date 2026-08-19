import { afterEach, describe, expect, it, vi } from "vitest";

import { loadInitialData } from "./bootstrap";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("loadInitialData", () => {
  it("establishes the session before requesting protected resources", async () => {
    const requests: string[] = [];
    let sessionEstablished = false;

    vi.stubGlobal(
      "fetch",
      async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const path = String(input);
        requests.push(path);

        if (path === "/api/v1/session") {
          expect(init?.method).toBe("POST");
          await Promise.resolve();
          sessionEstablished = true;
          return jsonResponse({
            id: "ses-test",
            ownerID: "usr-test",
            createdAt: "2026-08-19T00:00:00Z",
            expiresAt: "2026-08-20T00:00:00Z"
          }, 201);
        }

        if (!sessionEstablished) {
          throw new Error(`protected request raced session creation: ${path}`);
        }
        if (path === "/api/v1/catalog/models") {
          return jsonResponse({ models: [], profiles: [] });
        }
        if (path === "/api/v1/endpoints") {
          return jsonResponse({ endpoints: [] });
        }
        throw new Error(`unexpected request: ${path}`);
      }
    );

    const result = await loadInitialData();

    expect(result.session.id).toBe("ses-test");
    expect(result.catalog).toEqual({ models: [], profiles: [] });
    expect(result.endpoints).toEqual([]);
    expect(requests[0]).toBe("/api/v1/session");
    expect(requests.slice(1).sort()).toEqual([
      "/api/v1/catalog/models",
      "/api/v1/endpoints"
    ]);
  });
});

function jsonResponse(payload: unknown, status = 200): Response {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" }
  });
}
