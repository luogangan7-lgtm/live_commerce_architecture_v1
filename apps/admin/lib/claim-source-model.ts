// Owns the browser shape of the merchant comment source (claim-source HTTP interface,
// contracts/meta-claims-intake-v1.md §2 live.claim_sources): the closed GET/PUT parser.
// Non-goals: no fetching (claims-client.ts), no copy (claims-copy.ts), no input parsing or
// binding rule (Go resolves the pasted text, the store's Meta binding and the version CAS).
// Depends on nothing (pure, so node:test imports it directly).

export type ClaimSource = {
  id: string; platform: "facebook" | "instagram"; object: "page" | "instagram"; asset_id: string;
  source_object_id: string; private_reply: boolean; reply_locale: "zh-TW" | "zh-CN" | "en";
  active: boolean; version: number; verified: boolean; intake_count: number; intake_capped: number; updated_at: string;
};

const fields = ["id", "platform", "object", "asset_id", "source_object_id", "private_reply", "reply_locale",
  "active", "version", "verified", "intake_count", "intake_capped", "updated_at"];
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const count = (value: unknown, min: number) => Number.isSafeInteger(value) && (value as number) >= min;
const invalid = (): never => { throw new Error("invalid_claim_source_response"); };
function record(value: unknown, keys: string[]): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) return invalid();
  const row = value as Record<string, unknown>;
  if (Object.keys(row).sort().join(",") !== [...keys].sort().join(",")) invalid();
  return row;
}

/** Closed parser for one `source` object (GET envelope member and PUT response). */
export function parseClaimSource(value: unknown): ClaimSource {
  const row = record(value, fields);
  const platform = row.platform;
  if (typeof row.id !== "string" || !uuid.test(row.id) ||
    (platform !== "facebook" && platform !== "instagram") || row.object !== (platform === "facebook" ? "page" : "instagram") ||
    typeof row.asset_id !== "string" || !/^[A-Za-z0-9_.:-]{1,128}$/.test(row.asset_id) ||
    typeof row.source_object_id !== "string" || !/^[A-Za-z0-9_.:-]{1,128}$/.test(row.source_object_id) ||
    typeof row.private_reply !== "boolean" || typeof row.active !== "boolean" || typeof row.verified !== "boolean" ||
    (row.reply_locale !== "zh-TW" && row.reply_locale !== "zh-CN" && row.reply_locale !== "en") ||
    !count(row.version, 1) || !count(row.intake_count, 0) || !count(row.intake_capped, 0) ||
    typeof row.updated_at !== "string" || row.updated_at.length > 40 || !Number.isFinite(Date.parse(row.updated_at))) invalid();
  return row as ClaimSource;
}

export type SourcePlatform = ClaimSource["platform"];
export type ClaimSourceEnvelope = { source: ClaimSource | null; platforms: SourcePlatform[] };

/**
 * GET envelope: `source` null (nothing bound yet) or the bound source, plus `platforms`: the
 * store's enabled Meta binding providers, sorted and distinct (ruling p: the platform select
 * shows only when both are present).
 */
export function parseClaimSourceEnvelope(value: unknown): ClaimSourceEnvelope {
  const row = record(value, ["source", "platforms"]);
  const platforms = row.platforms;
  if (!Array.isArray(platforms) || platforms.length > 2 ||
    platforms.some((item, index) => (item !== "facebook" && item !== "instagram") || (index > 0 && platforms[index - 1] >= item))) invalid();
  return { source: row.source === null ? null : parseClaimSource(row.source), platforms: platforms as SourcePlatform[] };
}
