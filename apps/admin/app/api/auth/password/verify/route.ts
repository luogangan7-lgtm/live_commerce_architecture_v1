// POST /api/auth/password/verify → POST /v1/identity/password/complete (internal/identityhttp/password.go)
import { handleVerify, unsupported } from "../step1";

export const POST = handleVerify;
export const GET = unsupported;
export const PUT = unsupported;
export const DELETE = unsupported;
export const PATCH = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
