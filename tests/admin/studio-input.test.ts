import assert from "node:assert/strict";
import { test } from "node:test";
import { parseStudioDetail, parseStudioInput, parseStudioInputPrepared } from "../../apps/admin/lib/studio-model.ts";

const id = "22222222-2222-4222-8222-222222222222";
const date = "2026-09-27T12:34:56.123456789Z";
const prepared = {
  authorization_id: id, session_version: 1, start_before: date, environment: "MOCK",
  destinations: [{ ordinal: 1, provider: "facebook" }],
};
const input = {
  attempt_id: id, state: "RESERVED", admission_closed: false, close_reason: "",
  cleanup_held: false, can_stop: true, updated_at: date,
};
const missingFields = (row: Record<string, unknown>) => Object.keys(row).map((key) =>
  Object.fromEntries(Object.entries(row).filter(([name]) => name !== key)));

test("input readers accept null and exact public DTOs only", () => {
  assert.equal(parseStudioInput(null), null);
  assert.equal(parseStudioInputPrepared(null), null);
  assert.deepEqual(parseStudioInput(input), input);
  assert.deepEqual(parseStudioInputPrepared(prepared), prepared);
  for (const row of [undefined, [], {}, ...missingFields(input), { ...input, project_id: id },
    { ...input, attempt_id: "00000000-0000-0000-0000-000000000000" },
    { ...input, state: "ACTIVE" }, { ...input, close_reason: "private-secret" },
    { ...input, can_stop: 1 }, { ...input, cleanup_held: "false" },
    ...["admission_closed", "cleanup_held", "can_stop", "close_reason"].map((field) => ({ ...input, [field]: null })),
    { ...input, updated_at: "2026-02-30T00:00:00Z" },
    { ...input, admission_closed: true }, { ...input, state: "CLOSED" },
    { ...input, close_reason: "expired" }]) assert.throws(() => parseStudioInput(row));
  for (const row of [undefined, [], {}, ...missingFields(prepared),
    { prepared, project_id: id, credential_version: 1, endpoint_identity: "private" },
    { ...prepared, project_id: id }, { ...prepared, endpoint_identity: "private" },
    { ...prepared, credential_version: 1 }, { ...prepared, environment: "LIVE" },
    { ...prepared, authorization_id: "00000000-0000-0000-0000-000000000000" },
    ...[0, -1, 1.2, Number.MAX_SAFE_INTEGER + 1, "1"].map((session_version) => ({ ...prepared, session_version })),
    { ...prepared, destinations: [] }, { ...prepared, destinations: [{ ordinal: 2, provider: "facebook" }] },
    { ...prepared, destinations: [{ ordinal: 1, provider: "facebook", secret: "private" }] },
    { ...prepared, start_before: "2026-02-30T00:00:00Z" }]) assert.throws(() => parseStudioInputPrepared(row));
});

test("input can_stop is authoritative for joint liability, including held or closed input", () => {
  for (const state of ["CLOSING", "UNKNOWN", "CLOSED"]) {
    for (const cleanup_held of [false, true]) {
      for (const can_stop of [false, true]) {
        const row = { ...input, state, admission_closed: true, close_reason: "authorization_lost", cleanup_held, can_stop };
        assert.deepEqual(parseStudioInput(row), row);
      }
    }
  }
  assert.deepEqual(parseStudioInput({ ...input, state: "UNISSUED" }), { ...input, state: "UNISSUED" });
});

test("legacy Studio detail reuses the prepared parser but still binds draft version", () => {
  const row = {
    draft: { session_id: id, program_id: id, title: "Fixture", scheduled_at: null,
      aspect_ratio: "16:9", state: "DRAFT", version: 1, created_at: date, updated_at: date },
    prepared, attempt: null, can_manage: true, media_enabled: true,
  };
  assert.deepEqual(parseStudioDetail(row, id), row);
  // R1 ruling G2: media_enabled is a required boolean capability (planning-only Studio sends false).
  assert.deepEqual(parseStudioDetail({ ...row, prepared: null, media_enabled: false }, id), { ...row, prepared: null, media_enabled: false });
  assert.throws(() => { const { media_enabled: _, ...legacy } = row; parseStudioDetail(legacy, id); });
  assert.throws(() => parseStudioDetail({ ...row, media_enabled: "false" }, id));
  assert.throws(() => parseStudioDetail({ ...row, prepared: { ...prepared, session_version: 2 } }, id));
  assert.throws(() => parseStudioDetail({ ...row, prepared: { ...prepared, credential_version: 1 } }, id));
});
