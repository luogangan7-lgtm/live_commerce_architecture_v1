import { NextResponse, type NextRequest } from "next/server";
import {
  localeFromPath,
  localizedPath,
  resolveLocale,
} from "@live-commerce/i18n";
import { validOrdersQuery } from "./lib/orders-request";
import { validStudioQuery } from "./lib/studio-request";
import { claimsCollection, claimsSubpath } from "./lib/claims-request";

const uuid = "[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}";
const orderPath = new RegExp(`^/api/stores/${uuid}/orders(?:/${uuid})?$`);
const studioPath = new RegExp(`^/api/stores/${uuid}/live-sessions(?:/${uuid}(?:/(?:rehearsal/(?:start|stop)|input(?:/(?:start|token|prepared))?|${claimsSubpath}))?)?$`);
const storePrefix = new RegExp(`^/api/stores/${uuid}/`);
const studioPrefix = new RegExp(`^/api/stores/${uuid}/live-sessions(?:/|$)`);

// Guard raw order query syntax before Next normalizes it; auth stays in the route/Go.
// Other requests still receive only the existing locale routing preference.
export function proxy(request: NextRequest) {
  const path = request.nextUrl.pathname;
  if (path.startsWith("/api/")) {
    let decoded: string;
    try {
      decoded = decodeURIComponent(path);
    } catch {
      return NextResponse.next();
    }
    if (orderPath.test(decoded) && request.method === "GET") {
      if (!validOrdersQuery(request.url, !decoded.endsWith("/orders"))) {
        const requestID = crypto.randomUUID().replaceAll("-", "");
        return NextResponse.json(
          {
            code: "invalid_request",
            message: "Invalid request.",
            request_id: requestID,
            retryable: false,
            details: {},
          },
          {
            status: 422,
            headers: { "Cache-Control": "no-store", "X-Request-ID": requestID },
          },
        );
      }
    }
    if (studioPrefix.test(decoded) && (path !== decoded || !studioPath.test(decoded))) {
      const requestID = crypto.randomUUID().replaceAll("-", "");
      return NextResponse.json(
        { code: "not_found", message: "Resource not found.", request_id: requestID, retryable: false, details: {} },
        { status: 404, headers: { "Cache-Control": "private, no-store", "X-Request-ID": requestID } },
      );
    }
    if (studioPath.test(decoded) && !validStudioQuery(request.url, request.method === "GET" &&
      (decoded.endsWith("/live-sessions") || claimsCollection(decoded.replace(storePrefix, ""))))) {
      const requestID = crypto.randomUUID().replaceAll("-", "");
      return NextResponse.json(
        { code: "invalid_request", message: "Invalid request.", request_id: requestID, retryable: false, details: {} },
        { status: 422, headers: { "Cache-Control": "private, no-store", "X-Request-ID": requestID } },
      );
    }
    return NextResponse.next();
  }
  const locale = resolveLocale({
    pathname: path,
    preference: request.cookies.get("commerce_locale")?.value,
    acceptLanguage: request.headers.get("accept-language") ?? "",
    defaultLocale: "zh-CN",
  });
  let response: NextResponse;
  if (localeFromPath(path)) response = NextResponse.next();
  else {
    const target = request.nextUrl.clone();
    try {
      target.pathname = localizedPath(locale, path);
    } catch {
      return NextResponse.json({ code: "invalid_path" }, { status: 400 });
    }
    response = NextResponse.redirect(target);
  }
  response.cookies.set("commerce_locale", locale, {
    sameSite: "lax",
    path: "/",
    maxAge: 31536000,
    secure: request.nextUrl.protocol === "https:",
  });
  response.headers.set("Cache-Control", "private, no-store");
  return response;
}

export const config = {
  matcher: [
    "/((?!api|_next|demo-assets|favicon.ico|robots.txt).*)",
    // Only the store BFF has guards here. Matching /api/auth/* or /api/onboarding/* made Next buffer
    // their request bodies before the route's streaming size limit ran (identity-mock red run).
    "/api/stores/:path*",
  ],
};
