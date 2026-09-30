import assert from "node:assert/strict";
import { test } from "node:test";
import { logisticsRoute } from "../../apps/admin/lib/logistics-request.ts";

// taiwan-cvs-logistics-v1 §8/§16.5: BFF `/api/stores/{store}/logistics/*` grammar (the path here is the part after
// the store id, exactly what `[...resource]/route.ts` passes on).
test("LGR01 the grammar admits exactly the five logistics routes with their header policy", () => {
  const accepted: [string, string, string][] = [
    ["GET", "logistics/ecpay", "get"],
    ["PUT", "logistics/ecpay", "command"],
    ["POST", "logistics/ecpay/enabled", "command"],
    ["GET", "logistics/cvs-settings", "get"],
    ["PUT", "logistics/cvs-settings", "command"],
  ];
  for (const [method, path, kind] of accepted) assert.equal(logisticsRoute(method, path), kind, `${method} ${path}`);
});

test("LGR02 everything else is refused: other methods, extra segments, case, traversal, neighbours", () => {
  for (const [method, path] of [
    ["POST", "logistics/ecpay"], ["DELETE", "logistics/ecpay"], ["PATCH", "logistics/ecpay"], ["GET", "logistics/ecpay/enabled"],
    ["PUT", "logistics/ecpay/enabled"], ["POST", "logistics/cvs-settings"], ["DELETE", "logistics/cvs-settings"],
    ["GET", "logistics"], ["GET", "logistics/"], ["GET", "logistics/ecpay/"], ["GET", "logistics/ecpay/x"],
    ["POST", "logistics/ecpay/enabled/x"], ["POST", "logistics/ecpay/disable"], ["GET", "logistics/ECPAY"],
    ["GET", "logistics/Ecpay"], ["GET", "logistics/cvs_settings"], ["GET", "logistics/cvs-settings/x"],
    ["GET", "logistics/../orders"], ["GET", "logistics/ecpay?x=1"], ["GET", "logistics//ecpay"], ["GET", "/logistics/ecpay"],
    ["GET", "logistics/ecpay-keys"], ["GET", "logistics/keys"], ["GET", "order-actions"], ["HEAD", "logistics/ecpay"],
    ["OPTIONS", "logistics/cvs-settings"], ["GET", "x/logistics/ecpay"],
  ])
    assert.equal(logisticsRoute(method, path), null, `${method} ${path}`);
});
