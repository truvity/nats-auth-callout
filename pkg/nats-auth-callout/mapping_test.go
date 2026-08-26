package natsauthcallout

import (
	"testing"
)

func TestParseServiceAccount(t *testing.T) {
	tests := []struct {
		name          string
		username      string
		wantNamespace string
		wantName      string
		wantErr       bool
	}{
		{
			name:          "valid serviceaccount",
			username:      "system:serviceaccount:url-shortener:worker",
			wantNamespace: "url-shortener",
			wantName:      "worker",
		},
		{
			name:          "name containing extra colon keeps full remainder",
			username:      "system:serviceaccount:ci:runner:oddity",
			wantNamespace: "ci",
			wantName:      "runner:oddity",
		},
		{
			name:     "human user rejected",
			username: "kubernetes-admin",
			wantErr:  true,
		},
		{
			name:     "node identity rejected",
			username: "system:node:ip-10-0-0-1",
			wantErr:  true,
		},
		{
			name:     "missing name rejected",
			username: "system:serviceaccount:url-shortener",
			wantErr:  true,
		},
		{
			name:     "empty namespace rejected",
			username: "system:serviceaccount::worker",
			wantErr:  true,
		},
		{
			name:     "empty username rejected",
			username: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			namespace, name, err := ParseServiceAccount(tt.username)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseServiceAccount(%q) succeeded, want error", tt.username)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseServiceAccount(%q) failed: %v", tt.username, err)
			}

			if namespace != tt.wantNamespace || name != tt.wantName {
				t.Errorf("ParseServiceAccount(%q) = (%q, %q), want (%q, %q)",
					tt.username, namespace, name, tt.wantNamespace, tt.wantName)
			}
		})
	}
}

func TestAccountForNamespace(t *testing.T) {
	projects := map[string]struct{}{
		"url-shortener": {},
		"billing":       {},
	}

	tests := []struct {
		name        string
		namespace   string
		wantAccount string
		wantErr     bool
	}{
		{name: "listed project maps to itself", namespace: "url-shortener", wantAccount: "url-shortener"},
		{name: "other listed project maps to itself", namespace: "billing", wantAccount: "billing"},
		{name: "employee namespace maps to itself", namespace: "emp-otsar", wantAccount: "emp-otsar"},
		{name: "employee prefix without slug rejected", namespace: "emp-", wantErr: true},
		{name: "ci tenant namespace maps to itself", namespace: "ci-truvity-bar", wantAccount: "ci-truvity-bar"},
		{name: "ci tenant with dashed repo maps to itself", namespace: "ci-truvity-url-shortener", wantAccount: "ci-truvity-url-shortener"},
		{name: "ci prefix without suffix rejected", namespace: "ci-", wantErr: true},
		{name: "ci tenant without repo component rejected", namespace: "ci-truvity", wantErr: true},
		{name: "ci tenant with empty repo component rejected", namespace: "ci-truvity-", wantErr: true},
		{name: "ci tenant with empty org component rejected", namespace: "ci--bar", wantErr: true},
		{name: "arbitrary ci-prefixed namespace rejected", namespace: "ci-scratch", wantErr: true},
		{name: "legacy single ci namespace still maps", namespace: "ci", wantAccount: "ci"},
		{name: "unknown namespace rejected", namespace: "kube-system", wantErr: true},
		{name: "ci substring is not a ci tenant", namespace: "circus", wantErr: true},
		{name: "employee without dash rejected", namespace: "emp", wantErr: true},
		{name: "retired employee- prefix rejected", namespace: "employee-otsar", wantErr: true},
		{name: "empty namespace rejected", namespace: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account, err := AccountForNamespace(tt.namespace, projects)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("AccountForNamespace(%q) = %q, want error", tt.namespace, account)
				}

				return
			}

			if err != nil {
				t.Fatalf("AccountForNamespace(%q) failed: %v", tt.namespace, err)
			}

			if account != tt.wantAccount {
				t.Errorf("AccountForNamespace(%q) = %q, want %q", tt.namespace, account, tt.wantAccount)
			}
		})
	}
}

func TestAccountForNamespaceNoProjects(t *testing.T) {
	if _, err := AccountForNamespace("url-shortener", nil); err == nil {
		t.Error("AccountForNamespace with empty project list should reject a project namespace")
	}

	account, err := AccountForNamespace("emp-otsar", nil)
	if err != nil || account != "emp-otsar" {
		t.Errorf("AccountForNamespace(emp-otsar, nil) = (%q, %v), want (emp-otsar, nil)", account, err)
	}
}
