package provisioner

// Producer-side guard for the core -> control-plane provision request contract.
//
// cpProvisionRequest is the wire shape core POSTs to {cp}/cp/workspaces/provision.
// The control-plane decodes it into its OWN duplicated struct (wsProvisionRequest)
// in a SEPARATE repo, so a field added here silently does nothing if the CP lacks
// it (this is exactly how `template_assets` was dropped — RFC #2843 /
// project_saas_restart_re_stub_config).
//
// This test pins cpProvisionRequest's JSON tags to provision_request.contract.json
// (the SSOT). Adding or removing a wire field FAILS here until the contract is
// updated deliberately — at which point the reviewer must decide cp_consumes
// true/false, and the CP-side companion test (molecule-controlplane) enforces that
// every cp_consumes:true field is actually present on wsProvisionRequest.

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const provisionRequestContractPath = "provision_request.contract.json"

type provisionRequestContract struct {
	Fields map[string]struct {
		Type       string `json:"type"`
		CPConsumes bool   `json:"cp_consumes"`
		Note       string `json:"note"`
	} `json:"fields"`
}

// jsonWireTags returns the set of JSON wire field names for a struct type,
// stripping ",omitempty"/options and skipping "-" and untagged fields.
func jsonWireTags(t reflect.Type) map[string]bool {
	tags := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		raw := t.Field(i).Tag.Get("json")
		if raw == "" {
			continue
		}
		name := strings.Split(raw, ",")[0]
		if name == "" || name == "-" {
			continue
		}
		tags[name] = true
	}
	return tags
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestProvisionRequestContract_ProducerMatchesSSOT(t *testing.T) {
	data, err := os.ReadFile(provisionRequestContractPath)
	if err != nil {
		t.Fatalf("read contract %s: %v", provisionRequestContractPath, err)
	}
	var contract provisionRequestContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("parse contract %s: %v", provisionRequestContractPath, err)
	}
	if len(contract.Fields) == 0 {
		t.Fatalf("contract %s has no fields — refusing to pass a vacuous contract", provisionRequestContractPath)
	}

	structTags := jsonWireTags(reflect.TypeOf(cpProvisionRequest{}))

	contractFields := map[string]bool{}
	for name := range contract.Fields {
		contractFields[name] = true
	}

	var addedInStruct, missingFromStruct []string
	for name := range structTags {
		if !contractFields[name] {
			addedInStruct = append(addedInStruct, name)
		}
	}
	for name := range contractFields {
		if !structTags[name] {
			missingFromStruct = append(missingFromStruct, name)
		}
	}
	sort.Strings(addedInStruct)
	sort.Strings(missingFromStruct)

	if len(addedInStruct) > 0 {
		t.Errorf("cpProvisionRequest sends wire field(s) NOT in the contract: %v\n"+
			"  -> Add each to %s with an explicit cp_consumes (true if the CP's wsProvisionRequest already\n"+
			"     consumes it; false ONLY with a note explaining the dead channel). A field core sends that\n"+
			"     the CP does not consume is silently dropped — this is the template_assets failure class.",
			addedInStruct, provisionRequestContractPath)
	}
	if len(missingFromStruct) > 0 {
		t.Errorf("contract declares wire field(s) NOT present on cpProvisionRequest: %v\n"+
			"  -> If core intentionally stopped sending these, remove them from %s (and confirm the CP no\n"+
			"     longer requires them). Otherwise restore the struct field.",
			missingFromStruct, provisionRequestContractPath)
	}

	if t.Failed() {
		t.Logf("cpProvisionRequest json tags: %v", sortedKeys(structTags))
		t.Logf("contract fields:            %v", sortedKeys(contractFields))
	}
}

// TestProvisionRequestContract_DeadFieldsAreJustified ensures any cp_consumes:false
// field carries a note — so a dead wire field (sent-but-ignored) is an explicit,
// reviewed decision, never silent.
func TestProvisionRequestContract_DeadFieldsAreJustified(t *testing.T) {
	data, err := os.ReadFile(provisionRequestContractPath)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var contract provisionRequestContract
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatalf("parse contract: %v", err)
	}
	for name, f := range contract.Fields {
		if !f.CPConsumes && strings.TrimSpace(f.Note) == "" {
			t.Errorf("field %q is cp_consumes:false but has no `note` — a dead wire field MUST justify why "+
				"(remove the send, or document why it is intentionally unconsumed)", name)
		}
	}
}
