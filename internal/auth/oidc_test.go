package auth

import (
	"reflect"
	"sort"
	"testing"
)

func TestParseRolesArrayAndObject(t *testing.T) {
	arr := parseRoles([]any{"airllm_admin", "airllm_user"})
	sort.Strings(arr)
	if !reflect.DeepEqual(arr, []string{"airllm_admin", "airllm_user"}) {
		t.Fatalf("array claim: %v", arr)
	}
	// Zitadel-style object whose KEYS are the roles.
	obj := parseRoles(map[string]any{"airllm_admin": map[string]any{"org": "x"}})
	if len(obj) != 1 || obj[0] != "airllm_admin" {
		t.Fatalf("object claim: %v", obj)
	}
	if parseRoles(nil) != nil {
		t.Fatal("nil claim must yield no roles")
	}
}

func TestApplyRoleMap(t *testing.T) {
	got := applyRoleMap([]string{"admins", "devs"}, map[string]string{"admins": "airllm_admin", "devs": "airllm_user"})
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"airllm_admin", "airllm_user"}) {
		t.Fatalf("mapped roles: %v", got)
	}
	// no map -> identity
	id := applyRoleMap([]string{"airllm_admin"}, nil)
	if len(id) != 1 || id[0] != "airllm_admin" {
		t.Fatalf("identity map: %v", id)
	}
}
