package foundation_test

// CB13 (lane close, reviewer P2): scripts/ops/grant-r2-permissions.sql is an owner-approved production grant
// script that writes identity.store_grants and ops.audit_events; before this test nothing exercised it.
// Tier REAL_PG: the script is run UNMODIFIED through psql inside the labelled test PG container (the host has no
// psql; docker exec -i ... psql -f - is the same client). Disclosed fixtures: principals with explicit grant sets
// (the 0065 creator set WITHOUT the three R2 permissions = a store created before 0079).
// What it proves: granted=3 then granted=0; exactly one store.permissions_granted audit row; an abort with nothing
// written for a principal without the full creator set (ordinary member and a creator missing one permission); no
// change for any other principal; usage errors exit non-zero.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// cbgCreatorSet is the 0065 creator set (keep in step with identity.create_initial_store and the script).
var cbgCreatorSet = []string{"store:read", "audit:read", "audit:write", "catalog:read", "catalog:write",
	"inventory:read", "inventory:write", "inventory:reserve", "pricing:read", "pricing:write", "integration:read",
	"integration:manage", "orders:read", "live:read", "live:manage", "payments:refund", "fulfillment:write",
	"orders:export", "integration:execute"}

var cbgR2 = []string{"customers:read", "customers:privacy", "billing:manage"}

// cbgPsql runs the script as the PG superuser of the test container and returns combined output.
func cbgPsql(t *testing.T, container string, script []byte, vars ...string) (string, error) {
	t.Helper()
	args := []string{"exec", "-i", container, "psql", "-X", "-q", "-t", "-A", "-U", "postgres", "-d", "lc_foundation_test", "-v", "ON_ERROR_STOP=1"}
	for _, v := range vars {
		args = append(args, "-v", v)
	}
	cmd := exec.Command("docker", append(args, "-f", "-")...)
	cmd.Stdin = bytes.NewReader(script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestCustomersBillingCB13GrantScript(t *testing.T) {
	f := fixture(t)
	script, err := os.ReadFile("../../scripts/ops/grant-r2-permissions.sql")
	if err != nil {
		t.Fatal(err)
	}
	port := f.owner.Config().ConnConfig.Port
	ps, err := exec.Command("docker", "ps", "--format", "{{.Names}}|{{.Ports}}").Output()
	var container string
	for _, line := range strings.Split(strings.TrimSpace(string(ps)), "\n") {
		if n, ports, ok := strings.Cut(line, "|"); ok && strings.Contains(ports, fmt.Sprintf("127.0.0.1:%d->5432/tcp", port)) {
			container = n
		}
	}
	if err != nil || container == "" {
		t.Fatalf("cannot identify the test PG container for port %d: %v", port, err)
	}
	label, err := exec.Command("docker", "inspect", "-f", `{{index .Config.Labels "livecommerce.fixture"}}`, container).Output()
	if err != nil || strings.TrimSpace(string(label)) != container {
		t.Fatalf("refusing to touch an unlabelled container %s", container)
	}

	store, tenant := f.storeA1, f.tenantA
	creator, _ := lcPrincipal(t, f, tenant, []string{store}, cbgCreatorSet...)
	bystander, _ := lcPrincipal(t, f, tenant, []string{store}, cbgCreatorSet...)
	member, _ := lcPrincipal(t, f, tenant, []string{store}, "store:read", "orders:read")
	almost, _ := lcPrincipal(t, f, tenant, []string{store}, cbgCreatorSet[1:]...) // creator set minus store:read

	r2Count := func(principal string) int {
		return countRows(t, f.owner, `SELECT count(*) FROM identity.store_grants WHERE store_id=$1 AND principal_id=$2 AND permission=ANY($3)`, store, principal, cbgR2)
	}
	allGrants := func() int {
		return countRows(t, f.owner, `SELECT count(*) FROM identity.store_grants WHERE store_id=$1`, store)
	}
	audits := func() int {
		return countRows(t, f.owner, `SELECT count(*) FROM ops.audit_events WHERE store_id=$1 AND action='store.permissions_granted'`, store)
	}
	granted := regexp.MustCompile(`granted=(\d)`)
	run := func(principal string) (string, error) {
		return cbgPsql(t, container, script, "store_id="+store, "principal_id="+principal)
	}

	baseGrants, baseAudits := allGrants(), audits()
	out, err := run(creator)
	if err != nil || len(granted.FindStringSubmatch(out)) != 2 || granted.FindStringSubmatch(out)[1] != "3" {
		t.Fatalf("first run: err=%v out=%q, want granted=3", err, out)
	}
	if r2Count(creator) != 3 || allGrants() != baseGrants+3 || audits() != baseAudits+1 {
		t.Fatalf("after the first run: creator R2 grants=%d total grants %d->%d audits %d->%d, want 3, +3, +1", r2Count(creator), baseGrants, allGrants(), baseAudits, audits())
	}
	if out, err = run(creator); err != nil || granted.FindStringSubmatch(out) == nil || granted.FindStringSubmatch(out)[1] != "0" {
		t.Fatalf("second run: err=%v out=%q, want granted=0", err, out)
	}
	if allGrants() != baseGrants+3 || audits() != baseAudits+1 {
		t.Errorf("an idempotent rerun wrote: grants %d audits %d (want %d, %d)", allGrants(), audits(), baseGrants+3, baseAudits+1)
	}
	// Refusals write nothing: an ordinary member, and a principal one permission short of the creator set.
	for name, principal := range map[string]string{"ordinary member": member, "creator set minus one permission": almost} {
		grantsBefore, auditsBefore := allGrants(), audits()
		out, err := run(principal)
		if err == nil || !strings.Contains(out, "store-creator grant set") {
			t.Errorf("%s: err=%v out=%q, want a non-zero exit naming the missing creator grant set", name, err, out)
		}
		if allGrants() != grantsBefore || audits() != auditsBefore || r2Count(principal) != 0 {
			t.Errorf("%s: a refused run wrote (grants %d->%d, audits %d->%d, R2 grants %d)", name, grantsBefore, allGrants(), auditsBefore, audits(), r2Count(principal))
		}
	}
	// A principal that is not a member of the store's tenant, and an unknown store, are refused as well.
	if out, err := cbgPsql(t, container, script, "store_id="+store, "principal_id="+randomUUID()); err == nil || !strings.Contains(out, "not an active member") {
		t.Errorf("unknown principal: err=%v out=%q", err, out)
	}
	if out, err := cbgPsql(t, container, script, "store_id="+randomUUID(), "principal_id="+creator); err == nil || !strings.Contains(out, "store not found") {
		t.Errorf("unknown store: err=%v out=%q", err, out)
	}
	if r2Count(bystander) != 0 {
		t.Errorf("the run changed another principal: %d R2 grants", r2Count(bystander))
	}
	if out, err := cbgPsql(t, container, script); err == nil || !strings.Contains(out, "usage") {
		t.Errorf("missing variables: err=%v out=%q, want a usage error and a non-zero exit", err, out)
	}
}
