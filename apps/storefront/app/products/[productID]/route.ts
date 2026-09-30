// GET /products/{productID} -> 308 /zh-TW/products/{productID}. BFF: none (no Go call, no data read).
// Why: ads.request_for (migrations/0074, PRODUCT_TRAFFIC link_url) and ads.feed_rows (migrations/0080, Meta catalog `link`)
// freeze origin || '/products/' || product id with no locale segment, but the storefront only serves /{locale}/products/{id}
// (app/[locale]/layout.tsx notFound()s any other first segment). The static segment `products` wins over [locale], so this
// route makes every already-frozen ad link and feed link land on the page instead of a 404.
// zh-TW is the pilot market default (Taiwan buyers arriving from FB/IG); a later locale-aware redirect may replace it.
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const UUID = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;

export async function GET(
  _request: Request,
  { params }: { params: Promise<{ productID: string }> },
): Promise<Response> {
  const { productID } = await params;
  if (!UUID.test(productID)) return new Response("Not found", { status: 404 });
  // Relative Location: the redirect never depends on a caller-supplied Host header.
  return new Response(null, {
    status: 308,
    headers: { Location: `/zh-TW/products/${productID}`, "Cache-Control": "no-store" },
  });
}
