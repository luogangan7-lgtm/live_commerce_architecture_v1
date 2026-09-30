import type { NextConfig } from "next";

// Approved B product surface shares the verified buyer BFF. There are no public
// fixture routes. DNS/TLS/ingress remain independent deployment gates.
const config: NextConfig = {
  agentRules: false,
  devIndicators: false,
  poweredByHeader: false,
  reactStrictMode: true,
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
        ],
      },
      {
        // Next configuration headers override Route Handler headers. The fixed
        // neutral return supplies its own default-src/form-action none + style
        // hash and no-referrer, so the shopping policy must not match that path.
        source: "/((?!payment/return/?$).*)",
        headers: [
          { key: "Referrer-Policy", value: "same-origin" },
          {
            key: "Content-Security-Policy",
            value:
              "frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self' https://sandbox-api.payuni.com.tw/api/upp https://api.payuni.com.tw/api/upp https://logistics-stage.ecpay.com.tw/Express/map https://logistics.ecpay.com.tw/Express/map",
          },
        ],
      },
      {
        // The claim page holds a bearer link token in memory (live-keyword-claims-v1
        // §11.1): no Referer at all, and no scripts or connections beyond this origin.
        // Listed last so it overrides the shopping Referrer-Policy for this path only.
        source: "/:locale(zh-CN|zh-TW|en)/claim",
        headers: [
          { key: "Referrer-Policy", value: "no-referrer" },
          {
            key: "Content-Security-Policy",
            value:
              "frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'none'; connect-src 'self'",
          },
        ],
      },
    ];
  },
};
export default config;
