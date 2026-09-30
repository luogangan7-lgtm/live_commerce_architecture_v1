// BFF request grammar for the store-level logistics resources (contracts/taiwan-cvs-logistics-v1.md §8, §16.5):
// BFF `/api/stores/{store}/logistics/*` -> Go internal/httpapi/cvs.go. No generic proxying: every resource is
// listed. Order-level CVS resources (cvs-shipment, collection, pay-at-pickup-release) are in orders-request.ts.
// The route handler (`[...resource]/route.ts`, integrator hook) calls logisticsRoute() exactly like it calls
// orderActionRoute(): a null result means 404, otherwise the kind picks the header/body policy:
//   get      bare JSON read, no query/body/key
//   command  JSON body + Idempotency-Key (PUT ecpay, POST ecpay/enabled, PUT cvs-settings)
export type LogisticsKind = "get" | "command";
const routes: [string, RegExp, LogisticsKind][] = [
  ["GET", /^logistics\/ecpay$/, "get"],
  ["PUT", /^logistics\/ecpay$/, "command"],
  ["POST", /^logistics\/ecpay\/enabled$/, "command"],
  ["GET", /^logistics\/cvs-settings$/, "get"],
  ["PUT", /^logistics\/cvs-settings$/, "command"],
];
export function logisticsRoute(method: string, path: string): LogisticsKind | null {
  return routes.find(([m, re]) => m === method && re.test(path))?.[2] ?? null;
}
