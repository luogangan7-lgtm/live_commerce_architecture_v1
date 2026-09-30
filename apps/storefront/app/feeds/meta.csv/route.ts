// GET /feeds/meta.csv on the verified storefront host -> Go GET /v1/buyer/feeds/meta.csv (internal/attribution.FeedHandler,
// ads.feed_rows; contract meta-ads-v1 §7, AD10, C6). Public product feed for Meta Commerce Manager (scheduled feed URL).
//
// The store is never chosen by the caller: this route derives the origin from the Host header exactly like
// lib/buyer-server.ts candidateOrigin (lowercase DNS name, no port/IP/userinfo, https) and sends it to Go as
// X-Commerce-Storefront-Origin; Go resolves it through buyer.resolve_published_store. No query string is accepted, no
// caller header is forwarded, no cookie or bearer is sent, and the response is a bounded CSV stream with a fixed shape.
// It shares COMMERCE_BUYER_WEB_ENABLED / COMMERCE_BUYER_API_ORIGIN / COMMERCE_BUYER_BFF_KEY with the buyer BFF (the origin
// check is duplicated here because buyer-server.ts exports only handleBuyerRequest; integrator may export the helper).
import { isIP } from "node:net";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const MAX_BYTES = 8 * 1024 * 1024;
const TOKEN = /^[A-Za-z0-9_-]{43}$/;

function fail(status: number, code: string): Response {
  return Response.json(
    { code, message: code === "not_found" ? "Resource not found." : "Temporarily unavailable." },
    { status, headers: { "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" } },
  );
}

function candidateOrigin(request: Request): string | null {
  const host = request.headers.get("host");
  if (
    !host ||
    host.length > 253 ||
    host !== host.toLowerCase() ||
    host.includes(":") ||
    host.includes(",") ||
    host.includes("%") ||
    host.includes("@") ||
    host.endsWith(".") ||
    isIP(host)
  )
    return null;
  const labels = host.split(".");
  if (
    labels.length < 2 ||
    labels.some(
      (label) => label.length > 63 || !/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(label),
    )
  )
    return null;
  return `https://${host}`;
}

// apiOrigin/bffKey mirror lib/buyer-server.ts config(): https, or loopback http with a port, no path/userinfo/query.
function upstreamConfig(): { api: string; bff: string } | null {
  const enabled = process.env.COMMERCE_BUYER_WEB_ENABLED ?? "";
  if (enabled !== "1") return null;
  const raw = process.env.COMMERCE_BUYER_API_ORIGIN ?? "";
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    return null;
  }
  const loopback =
    url.protocol === "http:" &&
    (url.hostname === "127.0.0.1" || url.hostname === "localhost") &&
    !!url.port;
  if (
    raw !== url.origin ||
    (url.protocol !== "https:" && !loopback) ||
    url.username ||
    url.password ||
    url.search ||
    url.hash
  )
    return null;
  const bff = process.env.COMMERCE_BUYER_BFF_KEY ?? "";
  if (!TOKEN.test(bff)) return null;
  return { api: raw, bff };
}

async function readBounded(body: ReadableStream<Uint8Array> | null): Promise<Uint8Array | null> {
  if (!body) return null;
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > MAX_BYTES) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

export async function GET(request: Request): Promise<Response> {
  const url = new URL(request.url);
  if (url.search !== "" || url.pathname !== "/feeds/meta.csv") return fail(404, "not_found");
  const cfg = upstreamConfig();
  if (!cfg) return fail(503, "unavailable");
  const origin = candidateOrigin(request);
  if (!origin) return fail(404, "not_found");
  let upstream: Response;
  try {
    upstream = await fetch(`${cfg.api}/v1/buyer/feeds/meta.csv`, {
      method: "GET",
      headers: {
        Accept: "text/csv",
        "X-Commerce-Buyer-BFF-Key": cfg.bff,
        "X-Commerce-Storefront-Origin": origin,
      },
      cache: "no-store",
      redirect: "error",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(12000)]),
    });
  } catch {
    return fail(503, "unavailable");
  }
  // Anything but a CSV 200 is collapsed: an unpublished or unknown host is a 404, the rest a retryable 503 (no upstream text).
  if (upstream.status === 404 || upstream.status === 422) return fail(404, "not_found");
  const mime = (upstream.headers.get("content-type") ?? "").split(";", 1)[0].trim().toLowerCase();
  if (upstream.status !== 200 || mime !== "text/csv") return fail(503, "unavailable");
  const bytes = await readBounded(upstream.body);
  if (!bytes) return fail(503, "unavailable");
  return new Response(Buffer.from(bytes), {
    status: 200,
    headers: {
      "Content-Type": "text/csv; charset=utf-8",
      "Cache-Control": "public, max-age=900",
      "X-Content-Type-Options": "nosniff",
    },
  });
}
