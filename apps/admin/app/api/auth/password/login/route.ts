// POST /api/auth/password/login → POST /v1/identity/password/login (internal/identityhttp/password.go)
import { handleStep1, unsupported } from "../step1";

export const POST = (request: Request) => handleStep1(request, "login");
export const GET = unsupported;
export const PUT = unsupported;
export const DELETE = unsupported;
export const PATCH = unsupported;
export const OPTIONS = unsupported;
export const HEAD = unsupported;
